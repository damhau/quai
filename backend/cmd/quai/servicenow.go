package main

import (
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/servicenow"
)

// useServiceNow gives the request service ServiceNow's change requests when itsm.servicenow is
// configured; it reports whether it is.
func useServiceNow(cfg *config.Config, svc *requests.Service, network *netx.Network) (bool, error) {
	sn := cfg.ITSM.ServiceNow
	if sn == nil {
		return false, nil
	}
	c, err := servicenow.New(*sn, network, secretStore(cfg))
	if err != nil {
		return false, err
	}
	svc.UseServiceNow(sn, c)
	return true, nil
}
