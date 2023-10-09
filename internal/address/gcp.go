package address

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/doitintl/kubeip/internal/cloud"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/api/compute/v1"
)

const (
	operationDone           = "DONE" // operation status DONE
	inUseStatus             = "IN_USE"
	reservedStatus          = "RESERVED" // static IP addresses that are reserved but not currently in use
	defaultTimeout          = 10 * time.Minute
	defaultNetworkInterface = "nic0"
	accessConfigType        = "ONE_TO_ONE_NAT"
	accessConfigKind        = "compute#accessConfig"
)

type gcpAssigner struct {
	client  *compute.Service
	lister  cloud.Lister
	project string
	region  string
	logger  *logrus.Entry
}

func NewGCPAssigner(ctx context.Context, logger *logrus.Entry, project, region string) (Assigner, error) {
	// initialize Google Cloud client
	client, err := compute.NewService(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize Google Cloud client")
	}

	// get project ID from metadata server
	if project == "" {
		project, err = metadata.ProjectID()
		if err != nil {
			return nil, errors.Wrap(err, "failed to get project ID from metadata server")
		}
	}

	// get region from metadata server
	if region == "" {
		region, err = metadata.InstanceAttributeValue("cluster-location")
		if err != nil {
			return nil, errors.Wrap(err, "failed to get region from metadata server")
		}
		// if cluster-location is zone, extract region from zone
		if len(region) > 3 && region[len(region)-3] == '-' {
			region = region[:len(region)-3]
		}
	}

	return &gcpAssigner{
		client:  client,
		lister:  cloud.NewLister(client),
		project: project,
		region:  region,
		logger:  logger,
	}, nil
}

func (a *gcpAssigner) waitForOperation(op *compute.Operation, zone string, timeout time.Duration) error {
	if op == nil {
		a.logger.Warn("operation is nil")
		return nil
	}
	// Create a context that will be cancelled with timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var err error
	name := op.Name
	for op.Status != operationDone {
		// Pass the cancellable context to the Wait method
		op, err = a.client.ZoneOperations.Wait(a.project, zone, name).Context(ctx).Do()
		if err != nil {
			// If the context was cancelled, return a timeout error
			if errors.Is(err, context.Canceled) {
				return errors.New("operation timed out")
			}
			return errors.Wrapf(err, "failed to get operation %s", name)
		}
		// If the operation has an error, return it
		if op != nil && op.Error != nil {
			return errors.Errorf("operation %s failed with error %v", op.Name, op.Error.Errors)
		}
	}
	return nil
}

func (a *gcpAssigner) deleteInstanceAddress(instance *compute.Instance, zone string) error {
	// Check if the instance has at least one network interface
	if len(instance.NetworkInterfaces) == 0 {
		a.logger.WithField("instance", instance.Name).Info("instance has no network interfaces")
		return nil
	}
	// get instance network interface
	networkInterface := instance.NetworkInterfaces[0]
	// get instance network interface access config
	if len(networkInterface.AccessConfigs) == 0 {
		a.logger.WithField("instance", instance.Name).Info("instance network interface has no access configs")
		return nil
	}
	accessConfig := networkInterface.AccessConfigs[0]
	// get instance network interface access config name
	accessConfigName := accessConfig.Name
	// delete instance network interface access config
	a.logger.WithFields(logrus.Fields{
		"instance": instance.Name,
		"address":  accessConfig.NatIP,
	}).Infof("deleting ephemeral public IP address from instance")
	op, err := a.client.Instances.DeleteAccessConfig(a.project, zone, instance.Name, accessConfigName, networkInterface.Name).Do()
	if err != nil {
		return errors.Wrapf(err, "failed to delete access config %s from instance %s", accessConfigName, instance.Name)
	}
	// wait for operation to complete
	if err = a.waitForOperation(op, zone, defaultTimeout); err != nil {
		return errors.Wrapf(err, "failed to wait for operation %s", op.Name)
	}
	return nil
}

func (a *gcpAssigner) addInstanceAddress(instance *compute.Instance, zone string, address *compute.Address) error {
	// add instance network interface access config
	a.logger.WithFields(logrus.Fields{
		"instance": instance.Name,
		"address":  address.Address,
	}).Infof("adding reserved public IP address to instance")
	op, err := a.client.Instances.AddAccessConfig(a.project, zone, instance.Name, defaultNetworkInterface, &compute.AccessConfig{
		Name:  address.Name,
		Type:  accessConfigType,
		Kind:  accessConfigKind,
		NatIP: address.Address,
	}).Do()
	if err != nil {
		return errors.Wrapf(err, "failed to add access config %s to instance %s", address.Name, instance.Name)
	}
	// wait for operation to complete
	if err = a.waitForOperation(op, zone, defaultTimeout); err != nil {
		return errors.Wrapf(err, "failed to wait for operation %s", op.Name)
	}
	return nil
}

func (a *gcpAssigner) Assign(instanceID, zone string, filter []string, orderBy string) error {
	// check if instance already has a public static IP address assigned
	instance, err := a.client.Instances.Get(a.project, zone, instanceID).Do()
	if err != nil {
		return errors.Wrapf(err, "failed to get instance %s", instanceID)
	}
	assigned, err := a.listAddresses(nil, "", inUseStatus)
	if err != nil {
		return errors.Wrap(err, "failed to list assigned addresses")
	}
	if len(assigned) > 0 {
		for _, address := range assigned {
			for _, user := range address.Users {
				if user == instance.SelfLink {
					a.logger.WithFields(logrus.Fields{
						"instance": instanceID,
						"address":  address.Address,
					}).Infof("instance already has a static public IP address assigned")
					return nil
				}
			}
		}
	}

	// get available reserved public IP addresses
	addresses, err := a.listAddresses(filter, orderBy, reservedStatus)
	if err != nil {
		return errors.Wrap(err, "failed to list available addresses")
	}
	if len(addresses) == 0 {
		return errors.Errorf("no available addresses")
	}

	// delete current ephemeral public IP address
	if err = a.deleteInstanceAddress(instance, zone); err != nil {
		return errors.Wrap(err, "failed to delete current public IP address")
	}

	// assign first available static public IP address to the instance
	address := addresses[0]
	if err = a.addInstanceAddress(instance, zone, address); err != nil {
		return errors.Wrap(err, "failed to assign static public IP address")
	}

	return nil
}

func (a *gcpAssigner) listAddresses(filter []string, orderBy, status string) ([]*compute.Address, error) {
	call := a.lister.List(a.project, a.region)
	// Initialize filters with known filters
	filters := []string{
		fmt.Sprintf("(status=%s)", status),
		"(addressType=EXTERNAL)",
	}

	// filter addresses by provided filter: labels.key=value
	for _, f := range filter {
		filters = append(filters, fmt.Sprintf("(%s)", f))
	}
	// set the filter
	call = call.Filter(strings.Join(filters, " "))
	// sort addresses by
	if orderBy != "" {
		call = call.OrderBy(orderBy)
	}
	// get all addresses
	var addresses []*compute.Address
	for {
		list, err := call.Do()
		if err != nil {
			return nil, errors.Wrap(err, "failed to list available addresses")
		}
		addresses = append(addresses, list.Items...)
		if list.NextPageToken == "" {
			return addresses, nil
		}
		call = call.PageToken(list.NextPageToken)
	}
}
