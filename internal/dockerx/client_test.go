package dockerx

import (
	"testing"

	"github.com/docker/docker/api/types"
	networkTypes "github.com/docker/docker/api/types/network"
)

func TestContainerIPPrefersConfiguredNetwork(t *testing.T) {
	inspect := types.ContainerJSON{
		NetworkSettings: &types.NetworkSettings{
			Networks: map[string]*networkTypes.EndpointSettings{
				"bridge": {IPAddress: "172.17.0.2", NetworkID: "bridge-id"},
				"dcm":    {IPAddress: "172.20.0.5", NetworkID: "dcm-id"},
			},
		},
	}

	if got := containerIP(inspect, "dcm"); got != "172.20.0.5" {
		t.Fatalf("containerIP() = %q, want configured network IP", got)
	}
	if got := containerIP(inspect, "dcm-id"); got != "172.20.0.5" {
		t.Fatalf("containerIP() by network id = %q, want configured network IP", got)
	}
}

func TestContainerIPFallsBackWhenConfiguredNetworkHasNoIP(t *testing.T) {
	inspect := types.ContainerJSON{
		NetworkSettings: &types.NetworkSettings{
			DefaultNetworkSettings: types.DefaultNetworkSettings{IPAddress: "172.17.0.9"},
			Networks: map[string]*networkTypes.EndpointSettings{
				"dcm": {NetworkID: "dcm-id"},
			},
		},
	}

	if got := containerIP(inspect, "dcm"); got != "172.17.0.9" {
		t.Fatalf("containerIP() = %q, want default network fallback", got)
	}
}

func TestContainerConnectedToNetworkMatchesNameOrID(t *testing.T) {
	inspect := types.ContainerJSON{
		NetworkSettings: &types.NetworkSettings{
			Networks: map[string]*networkTypes.EndpointSettings{
				"dcm": {NetworkID: "dcm-id"},
			},
		},
	}

	if !containerConnectedToNetwork(inspect, "dcm", "") {
		t.Fatal("expected network name match")
	}
	if !containerConnectedToNetwork(inspect, "other-name", "dcm-id") {
		t.Fatal("expected network id match")
	}
	if containerConnectedToNetwork(inspect, "other-name", "other-id") {
		t.Fatal("did not expect unrelated network match")
	}
}
