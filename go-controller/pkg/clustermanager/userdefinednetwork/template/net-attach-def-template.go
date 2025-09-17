/*package template

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	OvnK8sCNIOverlay = "ovn-k8s-cni-overlay"

	FinalizerUserDefinedNetwork = "k8s.ovn.org/user-defined-network-protection"
	LabelUserDefinedNetwork     = "k8s.ovn.org/user-defined-network"

	cniVersion = "1.0.0"
)

type SpecGetter interface {
	GetTopology() userdefinednetworkv1.NetworkTopology
	GetLayer3() *userdefinednetworkv1.Layer3Config
	GetLayer2() *userdefinednetworkv1.Layer2Config
	GetLocalnet() *userdefinednetworkv1.LocalnetConfig
}

func RenderNetAttachDefManifest(obj client.Object, targetNamespace string) (*netv1.NetworkAttachmentDefinition, error) {
	if obj == nil {
		return nil, nil
	}

	if targetNamespace == "" {
		return nil, fmt.Errorf("namspace should not be empty")
	}

	var ownerRef metav1.OwnerReference
	var spec SpecGetter
	var networkName string
	switch o := obj.(type) {
	case *userdefinednetworkv1.UserDefinedNetwork:
		ownerRef = *metav1.NewControllerRef(obj, userdefinednetworkv1.SchemeGroupVersion.WithKind("UserDefinedNetwork"))
		spec = &o.Spec
		networkName = util.GenerateUDNNetworkName(targetNamespace, obj.GetName())
	case *userdefinednetworkv1.ClusterUserDefinedNetwork:
		ownerRef = *metav1.NewControllerRef(obj, userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork"))
		spec = &o.Spec.Network
		networkName = util.GenerateCUDNNetworkName(obj.GetName())
	default:
		return nil, fmt.Errorf("unknown type %T", obj)
	}

	nadName := util.GetNADName(targetNamespace, obj.GetName())

	nadSpec, err := RenderNADSpec(networkName, nadName, spec)
	if err != nil {
		return nil, err
	}

	return &netv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:            obj.GetName(),
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Labels:          renderNADLabels(obj),
			Finalizers:      []string{FinalizerUserDefinedNetwork},
		},
		Spec: *nadSpec,
	}, nil
}

func RenderNADSpec(networkName, nadName string, spec SpecGetter) (*netv1.NetworkAttachmentDefinitionSpec, error) {
	if err := validateTopology(spec); err != nil {
		return nil, fmt.Errorf("invalid topology specified: %w", err)
	}

	cniNetConf, err := renderCNINetworkConfig(networkName, nadName, spec)
	if err != nil {
		return nil, fmt.Errorf("failed to render CNI network config: %w", err)
	}
	cniNetConfRaw, err := json.Marshal(cniNetConf)
	if err != nil {
		return nil, err
	}

	return &netv1.NetworkAttachmentDefinitionSpec{
		Config: string(cniNetConfRaw),
	}, nil
}

// renderNADLabels copies labels from UDN to help RenderNADSpec
// function add those labels to corresponding NAD
func renderNADLabels(obj client.Object) map[string]string {
	labels := make(map[string]string)
	labels[LabelUserDefinedNetwork] = ""
	udnLabels := obj.GetLabels()
	if len(udnLabels) != 0 {
		maps.Copy(labels, udnLabels)
	}
	return labels
}

func validateTopology(spec SpecGetter) error {
	if spec.GetTopology() == userdefinednetworkv1.NetworkTopologyLayer3 && spec.GetLayer3() == nil ||
		spec.GetTopology() == userdefinednetworkv1.NetworkTopologyLayer2 && spec.GetLayer2() == nil ||
		spec.GetTopology() == userdefinednetworkv1.NetworkTopologyLocalnet && spec.GetLocalnet() == nil {
		return config.NewTopologyConfigMismatchError(string(spec.GetTopology()))
	}
	return nil
}

func renderCNINetworkConfig(networkName, nadName string, spec SpecGetter) (map[string]interface{}, error) {
	netConfSpec := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			CNIVersion: cniVersion,
			Type:       OvnK8sCNIOverlay,
			Name:       networkName,
		},
		NADName:  nadName,
		Topology: strings.ToLower(string(spec.GetTopology())),
	}

	switch spec.GetTopology() {
	case userdefinednetworkv1.NetworkTopologyLayer3:
		cfg := spec.GetLayer3()
		netConfSpec.Role = strings.ToLower(string(cfg.Role))
		netConfSpec.MTU = int(cfg.MTU)
		netConfSpec.Subnets = layer3SubnetsString(cfg.Subnets)
		netConfSpec.JoinSubnet = cidrString(renderJoinSubnets(cfg.Role, cfg.JoinSubnets))
	case userdefinednetworkv1.NetworkTopologyLayer2:
		cfg := spec.GetLayer2()
		if err := validateIPAM(cfg.IPAM); err != nil {
			return nil, err
		}
		if ipamEnabled(cfg.IPAM) && len(cfg.Subnets) == 0 {
			return nil, config.NewSubnetsRequiredError()
		}
		if !ipamEnabled(cfg.IPAM) && len(cfg.Subnets) > 0 {
			return nil, config.NewSubnetsMustBeUnsetError()
		}

		netConfSpec.Role = strings.ToLower(string(cfg.Role))
		netConfSpec.MTU = int(cfg.MTU)
		netConfSpec.AllowPersistentIPs = cfg.IPAM != nil && cfg.IPAM.Lifecycle == userdefinednetworkv1.IPAMLifecyclePersistent
		netConfSpec.Subnets = cidrString(cfg.Subnets)
		netConfSpec.JoinSubnet = cidrString(renderJoinSubnets(cfg.Role, cfg.JoinSubnets))
	case userdefinednetworkv1.NetworkTopologyLocalnet:
		cfg := spec.GetLocalnet()
		netConfSpec.Role = strings.ToLower(string(cfg.Role))
		netConfSpec.MTU = localnetMTU(cfg.MTU)
		netConfSpec.AllowPersistentIPs = cfg.IPAM != nil && cfg.IPAM.Lifecycle == userdefinednetworkv1.IPAMLifecyclePersistent
		netConfSpec.Subnets = cidrString(cfg.Subnets)
		netConfSpec.ExcludeSubnets = cidrString(cfg.ExcludeSubnets)
		netConfSpec.PhysicalNetworkName = cfg.PhysicalNetworkName

		if cfg.VLAN != nil && cfg.VLAN.Access != nil {
			netConfSpec.VLANID = int(cfg.VLAN.Access.ID)
		}
	}
	if netConfSpec.AllowPersistentIPs && !config.OVNKubernetesFeature.EnablePersistentIPs {
		return nil, fmt.Errorf("allowPersistentIPs is set but persistentIPs is Disabled")
	}

	if err := util.ValidateNetConf(nadName, netConfSpec); err != nil {
		return nil, err
	}
	if _, err := util.NewNetInfo(netConfSpec); err != nil {
		return nil, err
	}

	// Since 'ovncnitypes.NetConf' type and its embedded 'cnitypes.NetConf' type has
	// parameters that defined with 'ommitempty' JSON tag option but not as pointer,
	// they will always present in the marshaed JSON, making the UDN NAD spec config
	// having unexpected fields (e.g.:IPAM, RuntimeConfig).
	// Generating the net-conf JSON string using 'map[string]struct{}' provide the
	// expected result.
	cniNetConf := map[string]interface{}{
		"cniVersion":       cniVersion,
		"type":             OvnK8sCNIOverlay,
		"name":             networkName,
		"netAttachDefName": nadName,
		"topology":         netConfSpec.Topology,
		"role":             netConfSpec.Role,
	}
	if mtu := netConfSpec.MTU; mtu > 0 {
		cniNetConf["mtu"] = mtu
	}
	if len(netConfSpec.JoinSubnet) > 0 {
		cniNetConf["joinSubnet"] = netConfSpec.JoinSubnet
	}
	if len(netConfSpec.Subnets) > 0 {
		cniNetConf["subnets"] = netConfSpec.Subnets
	}
	if netConfSpec.AllowPersistentIPs {
		cniNetConf["allowPersistentIPs"] = netConfSpec.AllowPersistentIPs
	}
	if netConfSpec.PhysicalNetworkName != "" {
		cniNetConf["physicalNetworkName"] = netConfSpec.PhysicalNetworkName
	}
	if len(netConfSpec.ExcludeSubnets) > 0 {
		cniNetConf["excludeSubnets"] = netConfSpec.ExcludeSubnets
	}
	if netConfSpec.VLANID != 0 {
		cniNetConf["vlanID"] = netConfSpec.VLANID
	}
	return cniNetConf, nil
}

func localnetMTU(desiredMTU int32) int {
	// The MTU for localnet topology should be as the default MTU (1500) because the underlay
	// is not part of the SDN and compensating for the SDN overhead (100) is not required.
	mtu := config.Default.MTU + 100
	if desiredMTU > 0 {
		mtu = int(desiredMTU)
	}

	return mtu
}

func ipamEnabled(ipam *userdefinednetworkv1.IPAMConfig) bool {
	return ipam == nil || ipam.Mode == "" || ipam.Mode == userdefinednetworkv1.IPAMEnabled
}

func validateIPAM(ipam *userdefinednetworkv1.IPAMConfig) error {
	if ipam == nil {
		return nil
	}
	if ipam.Lifecycle == userdefinednetworkv1.IPAMLifecyclePersistent && !ipamEnabled(ipam) {
		return config.NewIPAMLifecycleNotSupportedError()
	}
	return nil
}

func renderJoinSubnets(role userdefinednetworkv1.NetworkRole, joinSubnetes []userdefinednetworkv1.CIDR) []userdefinednetworkv1.CIDR {
	if role != userdefinednetworkv1.NetworkRolePrimary {
		return nil
	}

	if len(joinSubnetes) == 0 {
		return []userdefinednetworkv1.CIDR{types.UserDefinedPrimaryNetworkJoinSubnetV4, types.UserDefinedPrimaryNetworkJoinSubnetV6}
	}

	return joinSubnetes
}

// layer3SubnetsString converts Layer3Subnet slice to comma seperated string
// (e.g.: "10.100.0.0/24/16, 10.200.0.0/24, ...").
// In case a Layer3Subent's HostSubnet is '0' or not specified it will not be
// appended becase it will result in an invalid format (e.g.: "10.200.0.0/24/0").
func layer3SubnetsString(subnets []userdefinednetworkv1.Layer3Subnet) string {
	var cidrs []string
	for _, subnet := range subnets {
		if subnet.HostSubnet > 0 {
			cidrs = append(cidrs, fmt.Sprintf("%s/%d", subnet.CIDR, subnet.HostSubnet))
		} else {
			cidrs = append(cidrs, string(subnet.CIDR))
		}
	}
	return strings.Join(cidrs, ",")
}

type cidr interface {
	userdefinednetworkv1.DualStackCIDRs | []userdefinednetworkv1.CIDR
}

func cidrString[T cidr](subnets T) string {
	var cidrs []string
	for _, subnet := range subnets {
		cidrs = append(cidrs, string(subnet))
	}
	return strings.Join(cidrs, ",")
}

func GetSpec(obj client.Object) SpecGetter {
	switch o := obj.(type) {
	case *userdefinednetworkv1.UserDefinedNetwork:
		return &o.Spec
	case *userdefinednetworkv1.ClusterUserDefinedNetwork:
		return &o.Spec.Network
	default:
		panic(fmt.Sprintf("unknown type %T", obj))
	}
}*/

package template

import (
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
	
	cnitypes "github.com/containernetworking/cni/pkg/types"
	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	OvnK8sCNIOverlay                = "ovn-k8s-cni-overlay"
	FinalizerUserDefinedNetwork     = "k8s.ovn.org/user-defined-network-protection"
	LabelUserDefinedNetwork         = "k8s.ovn.org/user-defined-network"
	cniVersion                      = "1.0.0"
	
	// VLAN mode constants
	VLANModeAccess = "access"
	VLANModeTrunk  = "trunk"
)

type SpecGetter interface {
	GetTopology() userdefinednetworkv1.NetworkTopology
	GetLayer3() *userdefinednetworkv1.Layer3Config
	GetLayer2() *userdefinednetworkv1.Layer2Config
	GetLocalnet() *userdefinednetworkv1.LocalnetConfig
}

// ParseVLANRange parses a VLAN specification (individual ID or range) and returns all VLAN IDs
func ParseVLANRange(vlanSpec string) ([]int, error) {
	vlanSpec = strings.TrimSpace(vlanSpec)
	
	// Check if it's a range (e.g., "110-120")
	if strings.Contains(vlanSpec, "-") {
		parts := strings.Split(vlanSpec, "-")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid VLAN range format: %s", vlanSpec)
		}
		
		start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return nil, fmt.Errorf("invalid start VLAN ID in range %s: %v", vlanSpec, err)
		}
		
		end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, fmt.Errorf("invalid end VLAN ID in range %s: %v", vlanSpec, err)
		}
		
		if start < 1 || start > 4094 || end < 1 || end > 4094 {
			return nil, fmt.Errorf("VLAN IDs must be between 1 and 4094, got range %s", vlanSpec)
		}
		
		if start >= end {
			return nil, fmt.Errorf("start VLAN ID must be less than end VLAN ID in range %s", vlanSpec)
		}
		
		var vlans []int
		for i := start; i <= end; i++ {
			vlans = append(vlans, i)
		}
		return vlans, nil
	} else {
		// Single VLAN ID
		vlanID, err := strconv.Atoi(vlanSpec)
		if err != nil {
			return nil, fmt.Errorf("invalid VLAN ID: %s", vlanSpec)
		}
		
		if vlanID < 1 || vlanID > 4094 {
			return nil, fmt.Errorf("VLAN ID must be between 1 and 4094, got %d", vlanID)
		}
		
		return []int{vlanID}, nil
	}
}

// ValidateVLANSpecs validates a list of VLAN specifications
func ValidateVLANSpecs(vlanSpecs []string) error {
	seenVLANs := make(map[int]bool)
	
	for _, spec := range vlanSpecs {
		vlans, err := ParseVLANRange(spec)
		if err != nil {
			return err
		}
		
		// Check for duplicates
		for _, vlan := range vlans {
			if seenVLANs[vlan] {
				return fmt.Errorf("duplicate VLAN ID %d found", vlan)
			}
			seenVLANs[vlan] = true
		}
	}
	
	return nil
}

// ExpandVLANSpecs expands VLAN specifications into individual VLAN IDs
func ExpandVLANSpecs(vlanSpecs []string) ([]int, error) {
	var allVLANs []int
	
	for _, spec := range vlanSpecs {
		vlans, err := ParseVLANRange(spec)
		if err != nil {
			return nil, err
		}
		allVLANs = append(allVLANs, vlans...)
	}
	
	return allVLANs, nil
}

func RenderNetAttachDefManifest(obj client.Object, targetNamespace string) (*netv1.NetworkAttachmentDefinition, error) {
	if obj == nil {
		return nil, nil
	}
	if targetNamespace == "" {
		return nil, fmt.Errorf("namspace should not be empty")
	}

	var ownerRef metav1.OwnerReference
	var spec SpecGetter
	var networkName string

	switch o := obj.(type) {
	case *userdefinednetworkv1.UserDefinedNetwork:
		ownerRef = *metav1.NewControllerRef(obj, userdefinednetworkv1.SchemeGroupVersion.WithKind("UserDefinedNetwork"))
		spec = &o.Spec
		networkName = util.GenerateUDNNetworkName(targetNamespace, obj.GetName())
	case *userdefinednetworkv1.ClusterUserDefinedNetwork:
		ownerRef = *metav1.NewControllerRef(obj, userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork"))
		spec = &o.Spec.Network
		networkName = util.GenerateCUDNNetworkName(obj.GetName())
	default:
		return nil, fmt.Errorf("unknown type %T", obj)
	}

	nadName := util.GetNADName(targetNamespace, obj.GetName())
	nadSpec, err := RenderNADSpec(networkName, nadName, spec)
	if err != nil {
		return nil, err
	}

	return &netv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:            obj.GetName(),
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Labels:          renderNADLabels(obj),
			Finalizers:      []string{FinalizerUserDefinedNetwork},
		},
		Spec: *nadSpec,
	}, nil
}

func RenderNADSpec(networkName, nadName string, spec SpecGetter) (*netv1.NetworkAttachmentDefinitionSpec, error) {
	if err := validateTopology(spec); err != nil {
		return nil, fmt.Errorf("invalid topology specified: %w", err)
	}

	cniNetConf, err := renderCNINetworkConfig(networkName, nadName, spec)
	if err != nil {
		return nil, fmt.Errorf("failed to render CNI network config: %w", err)
	}

	cniNetConfRaw, err := json.Marshal(cniNetConf)
	if err != nil {
		return nil, err
	}

	return &netv1.NetworkAttachmentDefinitionSpec{
		Config: string(cniNetConfRaw),
	}, nil
}

// renderNADLabels copies labels from UDN to help RenderNADSpec
// function add those labels to corresponding NAD
func renderNADLabels(obj client.Object) map[string]string {
	labels := make(map[string]string)
	labels[LabelUserDefinedNetwork] = ""
	udnLabels := obj.GetLabels()
	if len(udnLabels) != 0 {
		maps.Copy(labels, udnLabels)
	}
	return labels
}

func validateTopology(spec SpecGetter) error {
	if spec.GetTopology() == userdefinednetworkv1.NetworkTopologyLayer3 && spec.GetLayer3() == nil ||
		spec.GetTopology() == userdefinednetworkv1.NetworkTopologyLayer2 && spec.GetLayer2() == nil ||
		spec.GetTopology() == userdefinednetworkv1.NetworkTopologyLocalnet && spec.GetLocalnet() == nil {
		return config.NewTopologyConfigMismatchError(string(spec.GetTopology()))
	}
	return nil
}

// renderVLANConfig converts the CRD VLAN configuration to CNI VLAN configuration
func renderVLANConfig(vlanConfig *userdefinednetworkv1.VLANConfig) (*ovncnitypes.VLANConfiguration, error) {
	if vlanConfig == nil {
		return nil, nil
	}
	
	cniVLAN := &ovncnitypes.VLANConfiguration{}
	
	switch vlanConfig.Mode {
	case userdefinednetworkv1.VLANModeAccess:
		if vlanConfig.Access == nil {
			return nil, fmt.Errorf("access VLAN configuration is required when mode is Access")
		}
		cniVLAN.Mode = VLANModeAccess
		cniVLAN.AccessVLAN = int(vlanConfig.Access.ID)
		
	case userdefinednetworkv1.VLANModeTrunk:
		if vlanConfig.Trunk == nil {
			return nil, fmt.Errorf("trunk VLAN configuration is required when mode is Trunk")
		}
		cniVLAN.Mode = VLANModeTrunk
		
		// Convert allowed VLANs from string slice (supporting ranges) to string slice
		cniVLAN.TrunkVLANs = make([]string, len(vlanConfig.Trunk.AllowedVLANs))
		copy(cniVLAN.TrunkVLANs, vlanConfig.Trunk.AllowedVLANs)
		
		// Validate VLAN specifications
		if err := ValidateVLANSpecs(cniVLAN.TrunkVLANs); err != nil {
			return nil, fmt.Errorf("invalid trunk VLANs: %w", err)
		}
		
		// Set native VLAN if specified
		if vlanConfig.Trunk.NativeVLAN != nil {
			cniVLAN.NativeVLAN = int(*vlanConfig.Trunk.NativeVLAN)
			
			// Verify native VLAN is in the expanded VLAN list
			expandedVLANs, err := ExpandVLANSpecs(cniVLAN.TrunkVLANs)
			if err != nil {
				return nil, fmt.Errorf("failed to expand VLAN specs: %w", err)
			}
			
			found := false
			for _, vlan := range expandedVLANs {
				if vlan == cniVLAN.NativeVLAN {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("native VLAN %d must be included in trunkVLANs list", cniVLAN.NativeVLAN)
			}
		}
		
	default:
		return nil, fmt.Errorf("unsupported VLAN mode: %s", vlanConfig.Mode)
	}
	
	return cniVLAN, nil
}

func renderCNINetworkConfig(networkName, nadName string, spec SpecGetter) (map[string]interface{}, error) {
	netConfSpec := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			CNIVersion: cniVersion,
			Type:       OvnK8sCNIOverlay,
			Name:       networkName,
		},
		NADName:  nadName,
		Topology: strings.ToLower(string(spec.GetTopology())),
	}

	switch spec.GetTopology() {
	case userdefinednetworkv1.NetworkTopologyLayer3:
		cfg := spec.GetLayer3()
		netConfSpec.Role = strings.ToLower(string(cfg.Role))
		netConfSpec.MTU = int(cfg.MTU)
		netConfSpec.Subnets = layer3SubnetsString(cfg.Subnets)
		netConfSpec.JoinSubnet = cidrString(renderJoinSubnets(cfg.Role, cfg.JoinSubnets))
	case userdefinednetworkv1.NetworkTopologyLayer2:
		cfg := spec.GetLayer2()
		if err := validateIPAM(cfg.IPAM); err != nil {
			return nil, err
		}
		if ipamEnabled(cfg.IPAM) && len(cfg.Subnets) == 0 {
			return nil, config.NewSubnetsRequiredError()
		}
		if !ipamEnabled(cfg.IPAM) && len(cfg.Subnets) > 0 {
			return nil, config.NewSubnetsMustBeUnsetError()
		}
		netConfSpec.Role = strings.ToLower(string(cfg.Role))
		netConfSpec.MTU = int(cfg.MTU)
		netConfSpec.AllowPersistentIPs = cfg.IPAM != nil && cfg.IPAM.Lifecycle == userdefinednetworkv1.IPAMLifecyclePersistent
		netConfSpec.Subnets = cidrString(cfg.Subnets)
		netConfSpec.JoinSubnet = cidrString(renderJoinSubnets(cfg.Role, cfg.JoinSubnets))
	case userdefinednetworkv1.NetworkTopologyLocalnet:
		cfg := spec.GetLocalnet()
		netConfSpec.Role = strings.ToLower(string(cfg.Role))
		netConfSpec.MTU = localnetMTU(cfg.MTU)
		netConfSpec.AllowPersistentIPs = cfg.IPAM != nil && cfg.IPAM.Lifecycle == userdefinednetworkv1.IPAMLifecyclePersistent
		netConfSpec.Subnets = cidrString(cfg.Subnets)
		netConfSpec.ExcludeSubnets = cidrString(cfg.ExcludeSubnets)
		netConfSpec.PhysicalNetworkName = cfg.PhysicalNetworkName
		
		// Handle VLAN configuration for both access and trunk modes
		if cfg.VLAN != nil {
			vlanConfig, err := renderVLANConfig(cfg.VLAN)
			if err != nil {
				return nil, fmt.Errorf("failed to render VLAN config: %w", err)
			}
			netConfSpec.VLAN = vlanConfig
		}
	}

	if netConfSpec.AllowPersistentIPs && !config.OVNKubernetesFeature.EnablePersistentIPs {
		return nil, fmt.Errorf("allowPersistentIPs is set but persistentIPs is Disabled")
	}

	// Validate VLAN configuration
	if netConfSpec.VLAN != nil {
		if err := netConfSpec.VLAN.ValidateVLAN(); err != nil {
			return nil, fmt.Errorf("invalid VLAN configuration: %w", err)
		}
	}

	if err := util.ValidateNetConf(nadName, netConfSpec); err != nil {
		return nil, err
	}

	if _, err := util.NewNetInfo(netConfSpec); err != nil {
		return nil, err
	}

	// Since 'ovncnitypes.NetConf' type and its embedded 'cnitypes.NetConf' type has
	// parameters that defined with 'ommitempty' JSON tag option but not as pointer,
	// they will always present in the marshaed JSON, making the UDN NAD spec config
	// having unexpected fields (e.g.:IPAM, RuntimeConfig).
	// Generating the net-conf JSON string using 'map[string]struct{}' provide the
	// expected result.
	cniNetConf := map[string]interface{}{
		"cniVersion":       cniVersion,
		"type":             OvnK8sCNIOverlay,
		"name":             networkName,
		"netAttachDefName": nadName,
		"topology":         netConfSpec.Topology,
		"role":             netConfSpec.Role,
	}

	if mtu := netConfSpec.MTU; mtu > 0 {
		cniNetConf["mtu"] = mtu
	}
	if len(netConfSpec.JoinSubnet) > 0 {
		cniNetConf["joinSubnet"] = netConfSpec.JoinSubnet
	}
	if len(netConfSpec.Subnets) > 0 {
		cniNetConf["subnets"] = netConfSpec.Subnets
	}
	if netConfSpec.AllowPersistentIPs {
		cniNetConf["allowPersistentIPs"] = netConfSpec.AllowPersistentIPs
	}
	if netConfSpec.PhysicalNetworkName != "" {
		cniNetConf["physicalNetworkName"] = netConfSpec.PhysicalNetworkName
	}
	if len(netConfSpec.ExcludeSubnets) > 0 {
		cniNetConf["excludeSubnets"] = netConfSpec.ExcludeSubnets
	}

	// Handle VLAN configuration - only new format, no backward compatibility
	if netConfSpec.VLAN != nil {
		vlanMap := map[string]interface{}{
			"mode": netConfSpec.VLAN.Mode,
		}
		
		if netConfSpec.VLAN.IsAccessMode() {
			vlanMap["accessVLAN"] = netConfSpec.VLAN.AccessVLAN
		} else if netConfSpec.VLAN.IsTrunkMode() {
			vlanMap["trunkVLANs"] = netConfSpec.VLAN.TrunkVLANs
			if netConfSpec.VLAN.NativeVLAN != 0 {
				vlanMap["nativeVLAN"] = netConfSpec.VLAN.NativeVLAN
			}
		}
		
		cniNetConf["vlan"] = vlanMap
	}

	return cniNetConf, nil
}

func localnetMTU(desiredMTU int32) int {
	// The MTU for localnet topology should be as the default MTU (1500) because the underlay
	// is not part of the SDN and compensating for the SDN overhead (100) is not required.
	mtu := config.Default.MTU + 100
	if desiredMTU > 0 {
		mtu = int(desiredMTU)
	}
	return mtu
}

func ipamEnabled(ipam *userdefinednetworkv1.IPAMConfig) bool {
	return ipam == nil || ipam.Mode == "" || ipam.Mode == userdefinednetworkv1.IPAMEnabled
}

func validateIPAM(ipam *userdefinednetworkv1.IPAMConfig) error {
	if ipam == nil {
		return nil
	}
	if ipam.Lifecycle == userdefinednetworkv1.IPAMLifecyclePersistent && !ipamEnabled(ipam) {
		return config.NewIPAMLifecycleNotSupportedError()
	}
	return nil
}

func renderJoinSubnets(role userdefinednetworkv1.NetworkRole, joinSubnetes []userdefinednetworkv1.CIDR) []userdefinednetworkv1.CIDR {
	if role != userdefinednetworkv1.NetworkRolePrimary {
		return nil
	}
	if len(joinSubnetes) == 0 {
		return []userdefinednetworkv1.CIDR{types.UserDefinedPrimaryNetworkJoinSubnetV4, types.UserDefinedPrimaryNetworkJoinSubnetV6}
	}
	return joinSubnetes
}

// layer3SubnetsString converts Layer3Subnet slice to comma seperated string
// (e.g.: "10.100.0.0/24/16, 10.200.0.0/24, ...").
// In case a Layer3Subent's HostSubnet is '0' or not specified it will not be
// appended becase it will result in an invalid format (e.g.: "10.200.0.0/24/0").
func layer3SubnetsString(subnets []userdefinednetworkv1.Layer3Subnet) string {
	var cidrs []string
	for _, subnet := range subnets {
		if subnet.HostSubnet > 0 {
			cidrs = append(cidrs, fmt.Sprintf("%s/%d", subnet.CIDR, subnet.HostSubnet))
		} else {
			cidrs = append(cidrs, string(subnet.CIDR))
		}
	}
	return strings.Join(cidrs, ",")
}

type cidr interface {
	userdefinednetworkv1.DualStackCIDRs | []userdefinednetworkv1.CIDR
}

func cidrString[T cidr](subnets T) string {
	var cidrs []string
	for _, subnet := range subnets {
		cidrs = append(cidrs, string(subnet))
	}
	return strings.Join(cidrs, ",")
}

func GetSpec(obj client.Object) SpecGetter {
	switch o := obj.(type) {
	case *userdefinednetworkv1.UserDefinedNetwork:
		return &o.Spec
	case *userdefinednetworkv1.ClusterUserDefinedNetwork:
		return &o.Spec.Network
	default:
		panic(fmt.Sprintf("unknown type %T", obj))
	}
}
