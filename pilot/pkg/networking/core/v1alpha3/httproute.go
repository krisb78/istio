// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha3

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	protobuf "google.golang.org/protobuf/proto"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/core/v1alpha3/envoyfilter"
	istio_route "istio.io/istio/pilot/pkg/networking/core/v1alpha3/route"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pilot/pkg/serviceregistry/provider"
	"istio.io/istio/pilot/pkg/util/sets"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/proto"
)

const (
	wildcardDomainPrefix     = "*."
	inboundVirtualHostPrefix = string(model.TrafficDirectionInbound) + "|http|"
)

// BuildHTTPRoutes produces a list of routes for the proxy
func (configgen *ConfigGeneratorImpl) BuildHTTPRoutes(
	node *model.Proxy,
	req *model.PushRequest,
	routeNames []string) ([]*discovery.Resource, model.XdsLogDetails) {
	routeConfigurations := make([]*discovery.Resource, 0)

	efw := req.Push.EnvoyFilters(node)
	hit, miss := 0, 0
	switch node.Type {
	case model.SidecarProxy:
		vHostCache := make(map[int][]*route.VirtualHost)
		// dependent envoyfilters' key, calculate in front once to prevent calc for each route.
		envoyfilterKeys := efw.Keys()
		for _, routeName := range routeNames {
			rc, cached := configgen.buildSidecarOutboundHTTPRouteConfig(node, req, routeName, vHostCache, efw, envoyfilterKeys)
			if cached && !features.EnableUnsafeAssertions {
				hit++
			} else {
				miss++
			}
			if rc == nil {
				emptyRoute := &route.RouteConfiguration{
					Name:             routeName,
					VirtualHosts:     []*route.VirtualHost{},
					ValidateClusters: proto.BoolFalse,
				}
				rc = &discovery.Resource{
					Name:     routeName,
					Resource: util.MessageToAny(emptyRoute),
				}
			}
			routeConfigurations = append(routeConfigurations, rc)
		}
	case model.Router:
		for _, routeName := range routeNames {
			rc := configgen.buildGatewayHTTPRouteConfig(node, req.Push, routeName)
			if rc != nil {
				rc = envoyfilter.ApplyRouteConfigurationPatches(networking.EnvoyFilter_GATEWAY, node, efw, rc)
				resource := &discovery.Resource{
					Name:     routeName,
					Resource: util.MessageToAny(rc),
				}
				routeConfigurations = append(routeConfigurations, resource)
			}
		}
	}
	if !features.EnableRDSCaching {
		return routeConfigurations, model.DefaultXdsLogDetails
	}
	return routeConfigurations, model.XdsLogDetails{AdditionalInfo: fmt.Sprintf("cached:%v/%v", hit, hit+miss)}
}

// buildSidecarInboundHTTPRouteConfig builds the route config with a single wildcard virtual host on the inbound path
// TODO: trace decorators, inbound timeouts
func (configgen *ConfigGeneratorImpl) buildSidecarInboundHTTPRouteConfig(
	node *model.Proxy, push *model.PushContext, instance *model.ServiceInstance, clusterName string) *route.RouteConfiguration {
	traceOperation := util.TraceOperation(string(instance.Service.Hostname), instance.ServicePort.Port)
	defaultRoute := istio_route.BuildDefaultHTTPInboundRoute(clusterName, traceOperation)

	inboundVHost := &route.VirtualHost{
		Name:    inboundVirtualHostPrefix + strconv.Itoa(instance.ServicePort.Port), // Format: "inbound|http|%d"
		Domains: []string{"*"},
		Routes:  []*route.Route{defaultRoute},
	}

	r := &route.RouteConfiguration{
		Name:             clusterName,
		VirtualHosts:     []*route.VirtualHost{inboundVHost},
		ValidateClusters: proto.BoolFalse,
	}
	efw := push.EnvoyFilters(node)
	r = envoyfilter.ApplyRouteConfigurationPatches(networking.EnvoyFilter_SIDECAR_INBOUND, node, efw, r)
	return r
}

// buildSidecarOutboundHTTPRouteConfig builds an outbound HTTP Route for sidecar.
// Based on port, will determine all virtual hosts that listen on the port.
func (configgen *ConfigGeneratorImpl) buildSidecarOutboundHTTPRouteConfig(
	node *model.Proxy,
	req *model.PushRequest,
	routeName string,
	vHostCache map[int][]*route.VirtualHost,
	efw *model.EnvoyFilterWrapper,
	efKeys []string,
) (*discovery.Resource, bool) {
	var virtualHosts []*route.VirtualHost
	listenerPort := 0
	useSniffing := false
	var err error
	if features.EnableProtocolSniffingForOutbound &&
		!strings.HasPrefix(routeName, model.UnixAddressPrefix) {
		index := strings.IndexRune(routeName, ':')
		if index != -1 {
			useSniffing = true
		}
		listenerPort, err = strconv.Atoi(routeName[index+1:])
	} else {
		listenerPort, err = strconv.Atoi(routeName)
	}

	if err != nil {
		// we have a port whose name is http_proxy or unix:///foo/bar
		// check for both.
		if routeName != model.RDSHttpProxy && !strings.HasPrefix(routeName, model.UnixAddressPrefix) {
			// TODO: This is potentially one place where envoyFilter ADD operation can be helpful if the
			// user wants to ship a custom RDS. But at this point, the match semantics are murky. We have no
			// object to match upon. This needs more thought. For now, we will continue to return nil for
			// unknown routes
			return nil, false
		}
	}

	var routeCache *istio_route.Cache
	var resource *discovery.Resource

	cacheHit := false
	if useSniffing && listenerPort != 0 {
		// Check if we have already computed the list of all virtual hosts for this port
		// If so, then  we simply have to return only the relevant virtual hosts for
		// this listener's host:port
		if vhosts, exists := vHostCache[listenerPort]; exists {
			virtualHosts = getVirtualHostsForSniffedServicePort(vhosts, routeName)
			cacheHit = true
		}
	}
	if !cacheHit {
		virtualHosts, resource, routeCache = BuildSidecarOutboundVirtualHosts(node, req.Push, routeName, listenerPort, efKeys, configgen.Cache)
		if resource != nil {
			return resource, true
		}
		if useSniffing && listenerPort > 0 {
			// only cache for tcp ports and not for uds
			vHostCache[listenerPort] = virtualHosts
		}

		// FIXME: This will ignore virtual services with hostnames that do not match any service in the registry
		// per api spec, these hostnames + routes should appear in the virtual hosts (think bookinfo.com and
		// productpage.ns1.svc.cluster.local). See the TODO in BuildSidecarOutboundVirtualHosts for the right solution
		if useSniffing {
			virtualHosts = getVirtualHostsForSniffedServicePort(virtualHosts, routeName)
		}
	}

	util.SortVirtualHosts(virtualHosts)

	if !useSniffing {
		// virtualhost envoyfilter can mutate this sharing config.
		catchAll := protobuf.Clone(node.CatchAllVirtualHost).(*route.VirtualHost)
		virtualHosts = append(virtualHosts, catchAll)
	}

	out := &route.RouteConfiguration{
		Name:             routeName,
		VirtualHosts:     virtualHosts,
		ValidateClusters: proto.BoolFalse,
	}

	// apply envoy filter patches
	out = envoyfilter.ApplyRouteConfigurationPatches(networking.EnvoyFilter_SIDECAR_OUTBOUND, node, efw, out)

	resource = &discovery.Resource{
		Name:     out.Name,
		Resource: util.MessageToAny(out),
	}

	if features.EnableRDSCaching && routeCache != nil {
		configgen.Cache.Add(routeCache, req, resource)
	}

	return resource, false
}

func BuildSidecarOutboundVirtualHosts(node *model.Proxy, push *model.PushContext,
	routeName string,
	listenerPort int,
	efKeys []string,
	xdsCache model.XdsCache) ([]*route.VirtualHost, *discovery.Resource, *istio_route.Cache) {
	var virtualServices []config.Config
	var services []*model.Service

	// Get the services from the egress listener.  When sniffing is enabled, we send
	// route name as foo.bar.com:8080 which is going to match against the wildcard
	// egress listener only. A route with sniffing would not have been generated if there
	// was a sidecar with explicit port (and hence protocol declaration). A route with
	// sniffing is generated only in the case of the catch all egress listener.
	egressListener := node.SidecarScope.GetEgressListenerForRDS(listenerPort, routeName)
	// We should never be getting a nil egress listener because the code that setup this RDS
	// call obviously saw an egress listener
	if egressListener == nil {
		return nil, nil, nil
	}

	services = egressListener.Services()
	// To maintain correctness, we should only use the virtualservices for
	// this listener and not all virtual services accessible to this proxy.
	virtualServices = egressListener.VirtualServices()

	// When generating RDS for ports created via the SidecarScope, we treat ports as HTTP proxy style ports
	// if ports protocol is HTTP_PROXY.
	if egressListener.IstioListener != nil && egressListener.IstioListener.Port != nil &&
		protocol.Parse(egressListener.IstioListener.Port.Protocol) == protocol.HTTP_PROXY {
		listenerPort = 0
	}

	servicesByName := make(map[host.Name]*model.Service)
	hostsByNamespace := make(map[string][]host.Name)
	for _, svc := range services {
		if listenerPort == 0 {
			// Take all ports when listen port is 0 (http_proxy or uds)
			// Expect virtualServices to resolve to right port
			servicesByName[svc.Hostname] = svc
			hostsByNamespace[svc.Attributes.Namespace] = append(hostsByNamespace[svc.Attributes.Namespace], svc.Hostname)
		} else if svcPort, exists := svc.Ports.GetByPort(listenerPort); exists {
			servicesByName[svc.Hostname] = &model.Service{
				Hostname:       svc.Hostname,
				DefaultAddress: svc.GetAddressForProxy(node),
				MeshExternal:   svc.MeshExternal,
				Resolution:     svc.Resolution,
				Ports:          []*model.Port{svcPort},
				Attributes: model.ServiceAttributes{
					Namespace:       svc.Attributes.Namespace,
					ServiceRegistry: svc.Attributes.ServiceRegistry,
				},
			}
			hostsByNamespace[svc.Attributes.Namespace] = append(hostsByNamespace[svc.Attributes.Namespace], svc.Hostname)
		}
	}

	// This is hack to keep consistent with previous behavior.
	if listenerPort != 80 {
		// only select virtualServices that matches a service
		virtualServices = model.SelectVirtualServices(virtualServices, hostsByNamespace)
	}

	var routeCache *istio_route.Cache

	if listenerPort > 0 {
		services = make([]*model.Service, 0, len(servicesByName))
		// sort services
		for _, svc := range servicesByName {
			services = append(services, svc)
		}
		sort.SliceStable(services, func(i, j int) bool {
			return services[i].Hostname <= services[j].Hostname
		})

		routeCache = &istio_route.Cache{
			RouteName:               routeName,
			ProxyVersion:            node.Metadata.IstioVersion,
			ClusterID:               string(node.Metadata.ClusterID),
			DNSDomain:               node.DNSDomain,
			DNSCapture:              bool(node.Metadata.DNSCapture),
			DNSAutoAllocate:         bool(node.Metadata.DNSAutoAllocate),
			ListenerPort:            listenerPort,
			Services:                services,
			VirtualServices:         virtualServices,
			DelegateVirtualServices: push.DelegateVirtualServicesConfigKey(virtualServices),
			EnvoyFilterKeys:         efKeys,
		}
	}

	// Get list of virtual services bound to the mesh gateway
	virtualHostWrappers := istio_route.BuildSidecarVirtualHostWrapper(routeCache, node, push, servicesByName, virtualServices, listenerPort)

	resource, exist := xdsCache.Get(routeCache)
	if exist && !features.EnableUnsafeAssertions {
		return nil, resource, routeCache
	}

	vHostPortMap := make(map[int][]*route.VirtualHost)
	vhosts := sets.Set{}
	vhdomains := sets.Set{}
	knownFQDN := sets.Set{}

	buildVirtualHost := func(hostname string, vhwrapper istio_route.VirtualHostWrapper, svc *model.Service) *route.VirtualHost {
		name := util.DomainName(hostname, vhwrapper.Port)
		if duplicateVirtualHost(name, vhosts) {
			// This means this virtual host has caused duplicate virtual host name.
			var msg string
			if svc == nil {
				msg = fmt.Sprintf("duplicate domain from virtual service: %s", name)
			} else {
				msg = fmt.Sprintf("duplicate domain from service: %s", name)
			}
			push.AddMetric(model.DuplicatedDomains, name, node.ID, msg)
			return nil
		}
		var domains []string
		var altHosts []string
		if svc == nil {
			domains = []string{util.IPv6Compliant(hostname), name}
		} else {
			domains, altHosts = generateVirtualHostDomains(svc, vhwrapper.Port, node)
		}
		dl := len(domains)
		domains = dedupeDomains(domains, vhdomains, altHosts, knownFQDN)
		if dl != len(domains) {
			var msg string
			if svc == nil {
				msg = fmt.Sprintf("duplicate domain from virtual service: %s", name)
			} else {
				msg = fmt.Sprintf("duplicate domain from service: %s", name)
			}
			// This means this virtual host has caused duplicate virtual host domain.
			push.AddMetric(model.DuplicatedDomains, name, node.ID, msg)
		}
		if len(domains) > 0 {
			return &route.VirtualHost{
				Name:                       name,
				Domains:                    domains,
				Routes:                     vhwrapper.Routes,
				IncludeRequestAttemptCount: true,
			}
		}

		return nil
	}

	for _, virtualHostWrapper := range virtualHostWrappers {
		for _, svc := range virtualHostWrapper.Services {
			name := util.DomainName(string(svc.Hostname), virtualHostWrapper.Port)
			knownFQDN.Insert(name, string(svc.Hostname))
		}
	}

	for _, virtualHostWrapper := range virtualHostWrappers {
		// If none of the routes matched by source, skip this virtual host
		if len(virtualHostWrapper.Routes) == 0 {
			continue
		}
		virtualHosts := make([]*route.VirtualHost, 0, len(virtualHostWrapper.VirtualServiceHosts)+len(virtualHostWrapper.Services))

		for _, hostname := range virtualHostWrapper.VirtualServiceHosts {
			if vhost := buildVirtualHost(hostname, virtualHostWrapper, nil); vhost != nil {
				virtualHosts = append(virtualHosts, vhost)
			}
		}

		for _, svc := range virtualHostWrapper.Services {
			if vhost := buildVirtualHost(string(svc.Hostname), virtualHostWrapper, svc); vhost != nil {
				virtualHosts = append(virtualHosts, vhost)
			}
		}
		vHostPortMap[virtualHostWrapper.Port] = append(vHostPortMap[virtualHostWrapper.Port], virtualHosts...)
	}

	var out []*route.VirtualHost
	if listenerPort == 0 {
		out = mergeAllVirtualHosts(vHostPortMap)
	} else {
		out = vHostPortMap[listenerPort]
	}

	return out, nil, routeCache
}

// duplicateVirtualHost checks whether the virtual host with the same name exists in the route.
func duplicateVirtualHost(vhost string, vhosts sets.Set) bool {
	if vhosts.Contains(vhost) {
		return true
	}
	vhosts.Insert(vhost)
	return false
}

// dedupeDomains removes the duplicate domains from the passed in domains.
func dedupeDomains(domains []string, vhdomains sets.Set, expandedHosts []string, knownFQDNs sets.Set) []string {
	temp := domains[:0]
	for _, d := range domains {
		if vhdomains.Contains(d) {
			continue
		}
		// Check if the domain is an "expanded" host, and its also a known FQDN
		// This prevents a case where a domain like "foo.com.cluster.local" gets expanded to "foo.com", overwriting
		// the real "foo.com"
		// This works by providing a list of domains that were added as expanding the DNS domain as part of expandedHosts,
		// and a list of known unexpanded FQDNs to compare against
		if util.ListContains(expandedHosts, d) && knownFQDNs.Contains(d) { // O(n) search, but n is at most 10
			continue
		}
		temp = append(temp, d)
		vhdomains.Insert(d)
	}
	return temp
}

// Returns the set of virtual hosts that correspond to the listener that has HTTP protocol detection
// setup. This listener should only get the virtual hosts that correspond to this service+port and not
// all virtual hosts that are usually supplied for 0.0.0.0:PORT.
func getVirtualHostsForSniffedServicePort(vhosts []*route.VirtualHost, routeName string) []*route.VirtualHost {
	var virtualHosts []*route.VirtualHost
	for _, vh := range vhosts {
		for _, domain := range vh.Domains {
			if domain == routeName {
				virtualHosts = append(virtualHosts, vh)
				break
			}
		}
	}

	if len(virtualHosts) == 0 {
		return virtualHosts
	}
	if len(virtualHosts) == 1 {
		virtualHosts[0].Domains = []string{"*"}
		return virtualHosts
	}
	if features.EnableUnsafeAssertions {
		panic(fmt.Sprintf("unexpectedly matched multiple virtual hosts for %v: %v", routeName, virtualHosts))
	}
	return virtualHosts
}

// generateVirtualHostDomains generates the set of domain matches for a service being accessed from
// a proxy node
func generateVirtualHostDomains(service *model.Service, port int, node *model.Proxy) ([]string, []string) {
	altHosts := GenerateAltVirtualHosts(string(service.Hostname), port, node.DNSDomain)
	domains := []string{util.IPv6Compliant(string(service.Hostname)), util.DomainName(string(service.Hostname), port)}
	domains = append(domains, altHosts...)

	if service.Resolution == model.Passthrough &&
		service.Attributes.ServiceRegistry == provider.Kubernetes {
		for _, domain := range domains {
			domains = append(domains, wildcardDomainPrefix+domain)
		}
	}

	svcAddr := service.GetAddressForProxy(node)
	if len(svcAddr) > 0 && svcAddr != constants.UnspecifiedIP {
		domains = append(domains, util.IPv6Compliant(svcAddr), util.DomainName(svcAddr, port))
	}
	return domains, altHosts
}

// GenerateAltVirtualHosts given a service and a port, generates all possible HTTP Host headers.
// For example, a service of the form foo.local.campus.net on port 80, with local domain "local.campus.net"
// could be accessed as http://foo:80 within the .local network, as http://foo.local:80 (by other clients
// in the campus.net domain), as http://foo.local.campus:80, etc.
// NOTE: When a sidecar in remote.campus.net domain is talking to foo.local.campus.net,
// we should only generate foo.local, foo.local.campus, etc (and never just "foo").
//
// - Given foo.local.campus.net on proxy domain local.campus.net, this function generates
// foo:80, foo.local:80, foo.local.campus:80, with and without ports. It will not generate
// foo.local.campus.net (full hostname) since its already added elsewhere.
//
// - Given foo.local.campus.net on proxy domain remote.campus.net, this function generates
// foo.local:80, foo.local.campus:80
//
// - Given foo.local.campus.net on proxy domain "" or proxy domain example.com, this
// function returns nil
func GenerateAltVirtualHosts(hostname string, port int, proxyDomain string) []string {
	if strings.Contains(proxyDomain, ".svc.") {
		return generateAltVirtualHostsForKubernetesService(hostname, port, proxyDomain)
	}

	var vhosts []string
	uniqueHostnameParts, sharedDNSDomainParts := getUniqueAndSharedDNSDomain(hostname, proxyDomain)

	// If there is no shared DNS name (e.g., foobar.com service on local.net proxy domain)
	// do not generate any alternate virtual host representations
	if len(sharedDNSDomainParts) == 0 {
		return nil
	}

	uniqueHostname := strings.Join(uniqueHostnameParts, ".")

	// Add the uniqueHost.
	vhosts = append(vhosts, uniqueHostname, util.DomainName(uniqueHostname, port))
	if len(uniqueHostnameParts) == 2 {
		// This is the case of uniqHostname having namespace already.
		dnsHostName := uniqueHostname + "." + sharedDNSDomainParts[0]
		vhosts = append(vhosts, dnsHostName, util.DomainName(dnsHostName, port))
	}
	return vhosts
}

func generateAltVirtualHostsForKubernetesService(hostname string, port int, proxyDomain string) []string {
	id := strings.Index(proxyDomain, ".svc.")
	ih := strings.Index(hostname, ".svc.")
	if ih > 0 { // Proxy and service hostname are in kube
		ns := strings.Index(hostname, ".")
		if ns+1 >= len(hostname) || ns+1 > ih {
			// Invalid domain
			return nil
		}
		if hostname[ns+1:ih] == proxyDomain[:id] {
			// Same namespace
			return []string{
				hostname[:ns],
				util.DomainName(hostname[:ns], port),
				hostname[:ih] + ".svc",
				util.DomainName(hostname[:ih]+".svc", port),
				hostname[:ih],
				util.DomainName(hostname[:ih], port),
			}
		}
		// Different namespace
		return []string{
			hostname[:ih],
			util.DomainName(hostname[:ih], port),
			hostname[:ih] + ".svc",
			util.DomainName(hostname[:ih]+".svc", port),
		}
	}
	// Proxy is in k8s, but service isn't. No alt hosts
	return nil
}

// mergeAllVirtualHosts across all ports. On routes for ports other than port 80,
// virtual hosts without an explicit port suffix (IP:PORT) should not be added
func mergeAllVirtualHosts(vHostPortMap map[int][]*route.VirtualHost) []*route.VirtualHost {
	var virtualHosts []*route.VirtualHost
	for p, vhosts := range vHostPortMap {
		if p == 80 {
			virtualHosts = append(virtualHosts, vhosts...)
		} else {
			for _, vhost := range vhosts {
				var newDomains []string
				for _, domain := range vhost.Domains {
					if strings.Contains(domain, ":") {
						newDomains = append(newDomains, domain)
					}
				}
				if len(newDomains) > 0 {
					vhost.Domains = newDomains
					virtualHosts = append(virtualHosts, vhost)
				}
			}
		}
	}
	return virtualHosts
}

// reverseArray returns its argument string array reversed
func reverseArray(r []string) []string {
	for i, j := 0, len(r)-1; i < len(r)/2; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return r
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// getUniqueAndSharedDNSDomain computes the unique and shared DNS suffix from a FQDN service name and
// the proxy's local domain with namespace. This is especially useful in Kubernetes environments, where
// a two services can have same name in different namespaces (e.g., foo.ns1.svc.cluster.local,
// foo.ns2.svc.cluster.local). In this case, if the proxy is in ns2.svc.cluster.local, then while
// generating alt virtual hosts for service foo.ns1 for the sidecars in ns2 namespace, we should generate
// foo.ns1, foo.ns1.svc, foo.ns1.svc.cluster.local and should not generate a virtual host called "foo" for
// foo.ns1 service.
// So given foo.ns1.svc.cluster.local and ns2.svc.cluster.local, this function will return
// foo.ns1, and svc.cluster.local.
// When given foo.ns2.svc.cluster.local and ns2.svc.cluster.local, this function will return
// foo, ns2.svc.cluster.local.
func getUniqueAndSharedDNSDomain(fqdnHostname, proxyDomain string) (partsUnique []string, partsShared []string) {
	// split them by the dot and reverse the arrays, so that we can
	// start collecting the shared bits of DNS suffix.
	// E.g., foo.ns1.svc.cluster.local -> local,cluster,svc,ns1,foo
	//       ns2.svc.cluster.local -> local,cluster,svc,ns2
	partsFQDN := strings.Split(fqdnHostname, ".")
	partsProxyDomain := strings.Split(proxyDomain, ".")
	partsFQDNInReverse := reverseArray(partsFQDN)
	partsProxyDomainInReverse := reverseArray(partsProxyDomain)
	var sharedSuffixesInReverse []string // pieces shared between proxy and svc. e.g., local,cluster,svc

	for i := 0; i < min(len(partsFQDNInReverse), len(partsProxyDomainInReverse)); i++ {
		if partsFQDNInReverse[i] == partsProxyDomainInReverse[i] {
			sharedSuffixesInReverse = append(sharedSuffixesInReverse, partsFQDNInReverse[i])
		} else {
			break
		}
	}

	if len(sharedSuffixesInReverse) == 0 {
		partsUnique = partsFQDN
	} else {
		// get the non shared pieces (ns1, foo) and reverse Array
		partsUnique = reverseArray(partsFQDNInReverse[len(sharedSuffixesInReverse):])
		partsShared = reverseArray(sharedSuffixesInReverse)
	}
	return
}
