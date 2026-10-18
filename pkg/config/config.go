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
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package config

import (
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// PoolBinding maps a GKE node pool selector to the value of the {LabelKey}-node-pool address label.
// Wildcard is false for an exact pool name. Wildcard bindings match pool names that start with Selector.
// The configured trailing * is not stored on a wildcard selector.
type PoolBinding struct {
	Selector string
	Wildcard bool
	IPTag    string
}

// Config kubeip configuration
type Config struct {
	LabelKey            string
	LabelValue          string
	NodePool            string
	ForceAssignment     bool
	AdditionalNodePools []PoolBinding
	Ticker              time.Duration
	AllNodePools        bool
	OrderByLabelKey     string
	OrderByDesc         bool
	CopyLabels          bool
	ClearLabels         bool
	DryRun              bool
}

func setConfigDefaults() {
	viper.SetDefault("LabelKey", "kubeip")
	viper.SetDefault("LabelValue", "reserved")
	viper.SetDefault("NodePool", "default-pool")
	viper.SetDefault("ForceAssignment", true)
	viper.SetDefault("ForceAssignment", true)
	viper.SetDefault("AdditionalNodePools", "")
	viper.SetDefault("Ticker", 5)
	viper.SetDefault("AllNodePools", false)
	viper.SetDefault("OrderByLabelKey", "priority")
	viper.SetDefault("OrderByDesc", true)
	viper.SetDefault("CopyLabels", true)
	viper.SetDefault("ClearLabels", false)
	viper.SetDefault("DryRun", false)
}

// NewConfig initialize kubeip configuration
func NewConfig() (*Config, error) {
	viper.SetEnvPrefix("kubeip")
	viper.AutomaticEnv()
	setConfigDefaults()

	c := Config{
		LabelKey:            viper.GetString("labelkey"),
		LabelValue:          viper.GetString("labelvalue"),
		NodePool:            viper.GetString("nodepool"),
		ForceAssignment:     viper.GetBool("forceassignment"),
		AdditionalNodePools: parseAdditionalNodePools(viper.GetString("additionalnodepools")),
		Ticker:              viper.GetDuration("ticker"),
		AllNodePools:        viper.GetBool("allnodepools"),
		OrderByLabelKey:     viper.GetString("orderbylabelkey"),
		OrderByDesc:         viper.GetBool("orderbydesc"),
		CopyLabels:          viper.GetBool("copylabels"),
		ClearLabels:         viper.GetBool("clearlabels"),
		DryRun:              viper.GetBool("dryrun"),
	}
	return &c, nil
}

// parseAdditionalNodePools splits a comma-separated list of node_pool or node_pool|ip_tag entries.
// A pool name with no | is an exact match whose address tag is that pool name.
// A selector ending in * is a prefix match only when an explicit tag is present.
func parseAdditionalNodePools(raw string) []PoolBinding {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var bindings []PoolBinding
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		selector := part
		tag := ""
		explicitTag := false
		if i := strings.Index(part, "|"); i >= 0 {
			selector = strings.TrimSpace(part[:i])
			tag = strings.TrimSpace(part[i+1:])
			explicitTag = true
		} else {
			tag = selector
		}
		wildcard := explicitTag && strings.HasSuffix(selector, "*")
		if wildcard {
			selector = strings.TrimSuffix(selector, "*")
		}
		if tag == "" || (selector == "" && !wildcard) {
			logrus.WithFields(logrus.Fields{"pkg": "kubeip", "function": "parseAdditionalNodePools"}).Warnf("Skipping additional node pool entry %q: selector and tag must both be non-empty", part)
			continue
		}
		bindings = append(bindings, PoolBinding{
			Selector: selector,
			Wildcard: wildcard,
			IPTag:    tag,
		})
	}
	return bindings
}

// MatchAdditionalPool resolves a GKE node pool name to an additional-pool IP tag.
// Exact selectors win over prefixes. A longer prefix wins over a shorter one.
// Equal matches keep the earlier binding. KUBEIP_NODEPOOL is not considered here.
func (c *Config) MatchAdditionalPool(pool string) (tag string, wildcard bool, ok bool) {
	bestLen := -1
	var best PoolBinding
	foundPrefix := false
	for _, binding := range c.AdditionalNodePools {
		if !binding.Wildcard {
			if strings.EqualFold(pool, binding.Selector) {
				return binding.IPTag, false, true
			}
			continue
		}
		if hasPrefixFold(pool, binding.Selector) && len(binding.Selector) > bestLen {
			best = binding
			bestLen = len(binding.Selector)
			foundPrefix = true
		}
	}
	if foundPrefix {
		return best.IPTag, true, true
	}
	return "", false, false
}

func hasPrefixFold(value string, prefix string) bool {
	if len(prefix) > len(value) {
		return false
	}
	return strings.EqualFold(value[:len(prefix)], prefix)
}
