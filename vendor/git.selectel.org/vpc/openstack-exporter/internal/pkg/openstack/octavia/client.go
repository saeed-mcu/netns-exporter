package octavia

import (
	"net/http"

	"git.selectel.org/vpc/openstack-exporter/internal/pkg/config"
	"git.selectel.org/vpc/openstack-exporter/internal/pkg/openstack/common"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/pkg/errors"
)

// NewOctaviaV2Clients returns reference to instances of the Octavia V2 service
// clients that will be initialized for every region specified in the config.
// Clients will be saved into map with region names as keys.
func NewOctaviaV2Clients(opts *common.NewClientOpts) (map[string]gophercloud.ServiceClient, error) {
	regions := config.Config.OpenStack.Octavia.Regions
	clients := make(map[string]gophercloud.ServiceClient, len(regions))

	for _, region := range regions {
		client, err := openstack.NewLoadBalancerV2(opts.Provider, gophercloud.EndpointOpts{
			Region:       region,
			Availability: gophercloud.Availability(opts.EndpointType),
		})
		if err != nil {
			return nil, errors.Wrapf(err, "error initializing Octavia client in %s region", region)
		}
		client.HTTPClient = http.Client{
			Timeout: opts.Timeout,
		}
		clients[region] = *client
	}

	return clients, nil
}
