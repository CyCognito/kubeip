// Copyright © 2021 DoiT International
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package kipcompute

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
	cfg "github.com/doitintl/kubeip/pkg/config"
	"github.com/doitintl/kubeip/pkg/types"
	"github.com/doitintl/kubeip/pkg/utils"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/api/compute/v0.beta"
	"google.golang.org/api/container/v1"
)

// ClusterName get GKE cluster name from metadata
func ClusterName() (string, error) {
	return metadata.InstanceAttributeValue("cluster-name")
}

// ClusterLocation get GKE cluster location from metadata
func ClusterLocation() (string, error) {
	return metadata.InstanceAttributeValue("cluster-location")
}

// ProjectName get GCP project name from metadata
func ProjectName() (string, error) {
	return metadata.ProjectID()
}

var (
	containerMu      sync.Mutex
	containerService *container.Service
	computeMu        sync.Mutex
	computeService   *compute.Service
)

func getContainerService() (*container.Service, error) {
	containerMu.Lock()
	defer containerMu.Unlock()
	if containerService != nil {
		return containerService, nil
	}
	svc, err := container.NewService(context.Background())
	if err != nil {
		return nil, err
	}
	containerService = svc
	return containerService, nil
}

func getComputeService() (*compute.Service, error) {
	computeMu.Lock()
	defer computeMu.Unlock()
	if computeService != nil {
		return computeService, nil
	}
	svc, err := compute.NewService(context.Background())
	if err != nil {
		return nil, err
	}
	computeService = svc
	return computeService, nil
}

// ListClusterNodePoolNames returns the node pool names on this cluster.
// The names come from clusters.get, which is enough to turn a prefix rule into exact pools
// without listing every Kubernetes node.
func ListClusterNodePoolNames(projectID, location, cluster string) ([]string, error) {
	containerService, err := getContainerService()
	if err != nil {
		return nil, readNodePoolsError(projectID, location, cluster, err)
	}
	name := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", projectID, location, cluster)
	got, err := containerService.Projects.Locations.Clusters.Get(name).Do()
	if err != nil {
		return nil, readNodePoolsError(projectID, location, cluster, err)
	}
	names := make([]string, 0, len(got.NodePools))
	for _, pool := range got.NodePools {
		if pool != nil && pool.Name != "" {
			names = append(names, pool.Name)
		}
	}
	return names, nil
}

func readNodePoolsError(projectID, location, cluster string, err error) error {
	return errors.Wrapf(err, "can not read node pools for cluster %s in %s project %s", cluster, location, projectID)
}

func getPriorityOrder(address *compute.Address, config *cfg.Config) int {
	var defaultValue int
	if config.OrderByDesc {
		defaultValue = math.MinInt
	} else {
		defaultValue = math.MaxInt
	}

	strVal, ok := address.Labels[config.OrderByLabelKey]
	if ok {
		intVal, err := strconv.Atoi(strVal)
		if err != nil {
			logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "getPriorityOrder"}).Errorf("Address %s has errors. Failed to convert order by label value %s with value %s to integer", address.Name, config.OrderByLabelKey, strVal, err)
			return defaultValue
		}
		return intVal

	}

	return defaultValue
}

// GetAllAddresses retrieves all addresses matching the query.
func GetAllAddresses(projectID string, region string, filterJustReserved bool, config *cfg.Config) (*compute.AddressList, error) {
	return getAllAddresses(projectID, region, config.NodePool, filterJustReserved, config)
}

func addressFilter(pool string, config *cfg.Config) (string, error) {
	base := "(labels." + config.LabelKey + "=" + config.LabelValue + ")"
	if config.AllNodePools || strings.EqualFold(pool, config.NodePool) {
		return base + " AND  (-labels." + config.LabelKey + "-node-pool:*)", nil
	}
	tag, _, ok := config.MatchAdditionalPool(pool)
	if !ok {
		return "", fmt.Errorf("no additional node pool binding for pool %s", pool)
	}
	return base + " AND " + "(labels." + config.LabelKey + "-node-pool=" + tag + ")", nil
}

// listAllAddresses returns every address matching filter. Compute returns at most 500 per page.
func listAllAddresses(svc *compute.Service, projectID, region, filter string) ([]*compute.Address, error) {
	var items []*compute.Address
	err := svc.Addresses.List(projectID, region).Filter(filter).Pages(context.Background(), func(page *compute.AddressList) error {
		items = append(items, page.Items...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

func getAllAddresses(projectID string, region string, pool string, filterJustReserved bool, config *cfg.Config) (*compute.AddressList, error) {
	computeService, err := getComputeService()
	if err != nil {
		logrus.Error(err)
		return nil, err
	}
	filter, err := addressFilter(pool, config)
	if err != nil {
		logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "getAllAddresses"}).Error(err)
		return nil, err
	}

	var computedFilter string
	if filterJustReserved {
		computedFilter = "(status=RESERVED) AND " + filter
	} else {
		computedFilter = filter
	}

	items, err := listAllAddresses(computeService, projectID, region, computedFilter)
	if err != nil {
		logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "getAllAddresses"}).Errorf("Failed to list IP addresses: %q", err)
		return nil, err
	}
	addresses := &compute.AddressList{Items: items}

	// Right now the SDK does not support filter and order together, so we do it programmatically.
	sort.SliceStable(addresses.Items, func(i, j int) bool {
		address1 := addresses.Items[i]
		address2 := addresses.Items[j]
		val1 := getPriorityOrder(address1, config)
		val2 := getPriorityOrder(address2, config)
		if config.OrderByDesc {
			return val1 > val2
		}
		return val1 < val2
	})

	return addresses, nil
}

func findFreeAddress(projectID string, region string, pool string, config *cfg.Config) (types.IPAddress, error) {
	addresses, err := getAllAddresses(projectID, region, pool, true, config)
	if err != nil {
		logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "findFreeAddress"}).Errorf("Failed to list IP addresses in region %s: %q", region, err)
		return types.IPAddress{IP: "", Labels: map[string]string{}}, err
	}

	if len(addresses.Items) != 0 {
		address := addresses.Items[0]
		return types.IPAddress{IP: address.Address, Labels: address.Labels}, nil
	}
	return types.IPAddress{IP: "", Labels: map[string]string{}}, errors.New("no free address found")

}

// DeleteIP delete current IP on GKE node
func DeleteIP(projectID string, zone string, instance string, config *cfg.Config) error {
	computeService, err := getComputeService()
	if err != nil {
		logrus.Fatalf("Could not get authenticated client: %v", err)
	}
	inst, err := computeService.Instances.Get(projectID, zone, instance).Do()
	if err != nil {
		logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "DeleteIP"}).Errorf("Instance not found %s zone %s: %q", instance, zone, err)
		return err
	}
	if len(inst.NetworkInterfaces) > 0 && len(inst.NetworkInterfaces[0].AccessConfigs) > 0 {
		accessConfigName := inst.NetworkInterfaces[0].AccessConfigs[0].Name
		if config.DryRun {
			logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "DeleteIP"}).Infof("Deleted Access Config for %s zone %s new ip %s", instance, zone, accessConfigName)
		} else {
			op, err := computeService.Instances.DeleteAccessConfig(projectID, zone, instance, accessConfigName, "nic0").Do()
			if err != nil {
				logrus.Errorf("DeleteAccessConfig %q", err)
				return err
			}
			err = waitForCompilation(projectID, zone, op)
			if err != nil {
				return err
			}
		}

	}
	logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "DeleteIP"}).Infof("Deleted IP for %s zone %s", instance, zone)
	// Delete an prior tags.
	utils.TagNode(instance, types.IPAddress{IP: "0.0.0.0", Labels: map[string]string{}}, config)
	return nil
}

func addIP(projectID string, zone string, instance string, pool string, addr types.IPAddress, config *cfg.Config) error {
	computeService, err := getComputeService()
	if err != nil {
		logrus.Fatalf("Could not get authenticated client: %v", err)
	}

	if config.DryRun {
		logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "addIP"}).Infof("Added Access Config for %s zone %s new ip %s", instance, zone, addr.IP)
	} else {
		accessConfig := &compute.AccessConfig{
			Name:  "External NAT",
			Type:  "ONE_TO_ONE_NAT",
			NatIP: addr.IP,
			Kind:  "compute#accessConfig",
		}
		op, err := computeService.Instances.AddAccessConfig(projectID, zone, instance, "nic0", accessConfig).Do()
		if err != nil {
			logrus.Errorf("AddAccessConfig %q", err)
			return err
		}
		err = waitForCompilation(projectID, zone, op)
		if err != nil {
			return err
		}
	}

	logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "addIP"}).Infof("Added IP for %s zone %s new ip %s", instance, zone, addr.IP)
	return nil
}

func replaceIP(projectID string, zone string, instance string, pool string, config *cfg.Config) error {
	// wait for node to be ready with timeout of 5 minutes: this should be enough for the node to be ready
	err := utils.WaitForNodeReady(instance, 5*time.Minute)
	if err != nil {
		return errors.Wrap(err, "error waiting for node to be ready")
	}

	region := zone[:len(zone)-2]
	addr, err := findFreeAddress(projectID, region, pool, config)
	// Check if we found address.
	if err != nil {
		logrus.WithError(err).Error("can not find free address")
		return err
	}

	err = DeleteIP(projectID, zone, instance, config)
	if err != nil {
		logrus.WithError(err).Error("can not delete ip")
		return err
	}

	err = addIP(projectID, zone, instance, pool, addr, config)
	if err != nil {
		logrus.WithError(err).Error("can not add ip")
		return err
	}

	logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "replaceIP"}).Infof("Replaced IP for %s zone %s new ip %s", instance, zone, addr.IP)
	oldNode, err := utils.GetNodeByIP(addr.IP)
	if err != nil {
		utils.TagNode(oldNode, types.IPAddress{IP: "0.0.0.0", Labels: map[string]string{}}, config)
	}
	utils.TagNode(instance, addr, config)
	return nil

}

func waitForCompilation(projectID string, zone string, operation *compute.Operation) (err error) {
	computeService, err := getComputeService()
	if err != nil {
		logrus.Fatalf("Could not get create compute service: %v", err)
		return err
	}
	for {
		op, err := computeService.ZoneOperations.Get(projectID, zone, operation.Name).Do()
		if err != nil {
			logrus.Errorf("ZoneOperations.Get %q %s", err, operation.Name)
			return err
		}
		if strings.ToLower(op.Status) != "done" {
			time.Sleep(2 * time.Second)
		} else {
			return nil
		}
	}
}

// GetAddressUsedByInstance returns the IP used by this instance or the broadcast address otherwise.
func GetAddressUsedByInstance(projectID string, instance string, zone string, config *cfg.Config) (string, error) {
	computeService, err := getComputeService()
	if err != nil {
		logrus.Fatalf("Could not get create compute service: %v", err)
		return "", err
	}

	region := zone[:len(zone)-2]
	filter := "(labels." + config.LabelKey + "=" + config.LabelValue + ")"
	addresses, err := listAllAddresses(computeService, projectID, region, filter)
	if err != nil {
		logrus.Fatalf("Could not list addresses for instance %s: %v", instance, err)
		return "", err
	}

	for _, addr := range addresses {
		if len(addr.Users) > 0 && strings.Contains(addr.Users[0], instance) {
			return addr.Address, nil
		}
	}

	return "0.0.0.0", nil
}

// ListInUseReservedAddressResources returns the resource URLs attached to in-use kubeip addresses in one region.
func ListInUseReservedAddressResources(projectID string, region string, config *cfg.Config) ([]string, error) {
	svc, err := getComputeService()
	if err != nil {
		return nil, err
	}
	filter := "(status=IN_USE) AND (labels." + config.LabelKey + "=" + config.LabelValue + ")"
	addresses, err := listAllAddresses(svc, projectID, region, filter)
	if err != nil {
		return nil, err
	}
	resources := make([]string, 0, len(addresses))
	for _, addr := range addresses {
		if len(addr.Users) == 0 {
			continue
		}
		resources = append(resources, addr.Users[0])
	}
	return resources, nil
}

// InstanceOnReservedAddress reports whether instance appears in one of the reserved-address resource URLs.
func InstanceOnReservedAddress(resources []string, instance string) bool {
	for _, resource := range resources {
		if strings.Contains(resource, instance) {
			return true
		}
	}
	return false
}

// IsInstanceUsesReservedIP reports whether the GKE node is using a reserved IP.
// A list failure is returned as an error so the caller does not treat it as "no reserved IP".
func IsInstanceUsesReservedIP(projectID string, instance string, zone string, config *cfg.Config) (bool, error) {
	region := zone[:len(zone)-2]
	resources, err := ListInUseReservedAddressResources(projectID, region, config)
	if err != nil {
		return false, err
	}
	return InstanceOnReservedAddress(resources, instance), nil
}

// Kubeip replace GKE node IP
func Kubeip(instance <-chan types.Instance, config *cfg.Config) {
	for {
		inst := <-instance
		logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "Kubeip"}).Infof("Working on %s in zone %s", inst.Name, inst.Zone)
		usesReserved, err := IsInstanceUsesReservedIP(inst.ProjectID, inst.Name, inst.Zone, config)
		if err != nil {
			logrus.WithError(err).Errorf("can not check reserved IP for %s", inst.Name)
			continue
		}
		if !usesReserved {
			err := replaceIP(inst.ProjectID, inst.Zone, inst.Name, inst.Pool, config)
			if err != nil {
				logrus.WithError(err).Errorf("can not replace IP for %s", inst.Name)
				if cordonErr := utils.CordonNode(inst.Name, config); cordonErr != nil {
					logrus.WithError(cordonErr).Errorf("can not cordon node %s", inst.Name)
				}
				continue
			}
			if uncordonErr := utils.UncordonNode(inst.Name, config); uncordonErr != nil {
				logrus.WithError(uncordonErr).Errorf("can not uncordon node %s", inst.Name)
			}
		}
	}
}

func getAddressDetails(ip string, region string, projectID string, config *cfg.Config) (types.IPAddress, error) {
	computeService, err := getComputeService()
	if err != nil {
		logrus.Error(err)
		return types.IPAddress{IP: "", Labels: map[string]string{}}, err
	}
	filter := "address=" + "\"" + ip + "\""

	addresses, err := listAllAddresses(computeService, projectID, region, filter)
	if err != nil {
		logrus.Error(err)
		return types.IPAddress{IP: "", Labels: map[string]string{}}, err
	}

	if len(addresses) == 1 {
		address := addresses[0]
		logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "getAddressDetails"}).Infof("Node ip is reserved %s %s", ip, fmt.Sprint(address.Labels))
		return types.IPAddress{IP: address.Address, Labels: address.Labels}, nil
	}

	logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "getAddressDetails"}).Errorf("%d addresses returned for ip %s", len(addresses), ip)
	return types.IPAddress{IP: "", Labels: map[string]string{}}, fmt.Errorf("%d addresses returned for ip %s", len(addresses), ip)
}

func isAddressReserved(ip string, region string, projectID string, config *cfg.Config) bool {
	computeService, err := getComputeService()
	if err != nil {
		logrus.Error(err)
		return false
	}
	filter := "address=" + "\"" + ip + "\""
	addresses, err := listAllAddresses(computeService, projectID, region, filter)
	if err != nil {
		logrus.Error(err)
		return false
	}

	if len(addresses) != 0 {
		logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "isAddressReserved"}).Infof("Node ip is reserved %s", ip)
		return true
	}
	return false
}

// AddTagIfMissing add GKE node tag if missing
func AddTagIfMissing(projectID string, instance string, zone string, config *cfg.Config) {
	computeService, err := getComputeService()
	if err != nil {
		logrus.Fatalf("Could not get authenticated client: %v", err)
		return
	}
	inst, err := computeService.Instances.Get(projectID, zone, instance).Do()
	if err != nil {
		return
	}
	var ip string
	for _, config := range inst.NetworkInterfaces[0].AccessConfigs {
		if config.NatIP != "" {
			ip = config.NatIP
		}
	}
	region := zone[:len(zone)-2]
	if isAddressReserved(ip, region, projectID, config) {
		addressDetails, err := getAddressDetails(ip, region, projectID, config)
		if err != nil {
			return
		}
		logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "AddTagIfMissing"}).Infof("Tagging %s", instance)
		utils.TagNode(instance, addressDetails, config)
	}

}
