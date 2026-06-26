# API Reference

## Packages
- [netbird.io/v1alpha1](#netbirdiov1alpha1)


## netbird.io/v1alpha1

Package v1alpha1 contains API Schema definitions for the  v1alpha1 API group.

### Resource Types
- [ClusterProxy](#clusterproxy)
- [DNSRecord](#dnsrecord)
- [DNSZone](#dnszone)
- [Group](#group)
- [Network](#network)
- [NetworkResource](#networkresource)
- [NetworkRouter](#networkrouter)
- [ReverseProxyService](#reverseproxyservice)
- [SetupKey](#setupkey)
- [SidecarProfile](#sidecarprofile)



#### AccessRestrictions



AccessRestrictions are connection-level restrictions based on IP address or
geography, applied to the reverse-proxy service.



_Appears in:_
- [ReverseProxyServiceSpec](#reverseproxyservicespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `allowedCidrs` _string array_ | AllowedCidrs is a CIDR allowlist. If non-empty, only matching source IPs<br />are allowed. Evaluated before BlockedCidrs. |  | MaxItems: 64 <br />items:MaxLength: 43 <br />Optional: \{\} <br /> |
| `blockedCidrs` _string array_ | BlockedCidrs is a CIDR blocklist. Matching source IPs are rejected. |  | MaxItems: 64 <br />items:MaxLength: 43 <br />Optional: \{\} <br /> |
| `allowedCountries` _string array_ | AllowedCountries is an ISO 3166-1 alpha-2 country-code allowlist. If<br />non-empty, only these countries are permitted. |  | MaxItems: 250 <br />items:MaxLength: 2 <br />Optional: \{\} <br /> |
| `blockedCountries` _string array_ | BlockedCountries is an ISO 3166-1 alpha-2 country-code blocklist. |  | MaxItems: 250 <br />items:MaxLength: 2 <br />Optional: \{\} <br /> |


#### ClusterProxy



ClusterProxy is the Schema for the clusterproxies API





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `netbird.io/v1alpha1` | | |
| `kind` _string_ | `ClusterProxy` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ClusterProxySpec](#clusterproxyspec)_ |  |  | Required: \{\} <br /> |
| `status` _[ClusterProxyStatus](#clusterproxystatus)_ |  | \{ observedGeneration:-1 \} |  |


#### ClusterProxySpec



ClusterProxySpec defines the desired state of ClusterProxy.



_Appears in:_
- [ClusterProxy](#clusterproxy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `clusterName` _string_ | ClusterName is the name of the Kubernetes cluster. |  | Required: \{\} <br /> |
| `apiServer` _string_ | APIServer is the URL of the Kubernetes API server to proxy requests to. | https://kubernetes.default.svc.cluster.local | Required: \{\} <br /> |
| `serviceAccountName` _string_ | ServiceAccountName is a reference to the service account used for impersonation. |  | Required: \{\} <br /> |
| `groups` _[GroupReference](#groupreference) array_ | Groups are references to groups that the peer will be a part of. |  | Optional: \{\} <br /> |
| `replicas` _integer_ | Replicas is the number of proxy Deployment replicas. Each replica is a<br />separate peer sharing the <clusterName>.netbird-kubeapi-proxy DNS label,<br />so more replicas means HA for kubectl-over-mesh. Defaults to 3. | 3 | Minimum: 1 <br />Optional: \{\} <br /> |


#### ClusterProxyStatus



ClusterProxyStatus defines the observed state of ClusterProxy.



_Appears in:_
- [ClusterProxy](#clusterproxy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the last reconciled generation. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#condition-v1-meta) array_ | Conditions holds the conditions for the ClusterProxy. |  | Optional: \{\} <br /> |


#### ContainerOverride







_Appears in:_
- [SidecarProfileSpec](#sidecarprofilespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `image` _string_ | Image overrides the image used by the client. |  | Optional: \{\} <br /> |
| `env` _[EnvVar](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#envvar-v1-core) array_ |  |  | Optional: \{\} <br /> |
| `securityContext` _[SecurityContext](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#securitycontext-v1-core)_ |  |  | Optional: \{\} <br /> |
| `startupProbe` _[Probe](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#probe-v1-core)_ | StartupProbe overrides the startup probe for the sidecar container. |  | Optional: \{\} <br /> |
| `livenessProbe` _[Probe](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#probe-v1-core)_ | LivenessProbe overrides the liveness probe for the sidecar container. |  | Optional: \{\} <br /> |
| `readinessProbe` _[Probe](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#probe-v1-core)_ | ReadinessProbe overrides the readiness probe for the sidecar container. |  | Optional: \{\} <br /> |


#### CrossNamespaceReference







_Appears in:_
- [DNSRecordSpec](#dnsrecordspec)
- [NetworkResourceSpec](#networkresourcespec)
- [NetworkRouterSpec](#networkrouterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the referent. |  | Required: \{\} <br /> |
| `namespace` _string_ | Namespace of the referent. |  | Required: \{\} <br /> |


#### CrowdsecMode

_Underlying type:_ _string_

CrowdsecMode selects how the proxy cluster's CrowdSec IP-reputation check is
applied. Only effective when the proxy cluster supports CrowdSec.

_Validation:_
- Enum: [off observe enforce]

_Appears in:_
- [ReverseProxyServiceSpec](#reverseproxyservicespec)

| Field | Description |
| --- | --- |
| `off` |  |
| `observe` |  |
| `enforce` |  |


#### DNSRecord



DNSRecord is the Schema for the dnsrecords API. It is a thin mirror of a
single NetBird DNS record.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `netbird.io/v1alpha1` | | |
| `kind` _string_ | `DNSRecord` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[DNSRecordSpec](#dnsrecordspec)_ |  |  | Required: \{\} <br /> |
| `status` _[DNSRecordStatus](#dnsrecordstatus)_ |  | \{ observedGeneration:-1 \} |  |


#### DNSRecordSpec



DNSRecordSpec defines the desired state of DNSRecord. It mirrors the NetBird
DNS-record API (POST /api/dns/zones/{zone}/records) 1:1: a single record in a
zone. The controller adopts an existing record matching name+type+content
rather than recreating it (NetBird rejects duplicate records).



_Appears in:_
- [DNSRecord](#dnsrecord)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `zoneRef` _[CrossNamespaceReference](#crossnamespacereference)_ | ZoneRef references the DNSZone this record is created in. The zone must be<br />Ready; its status.zoneID identifies the NetBird zone. |  |  |
| `name` _string_ | Name is the record name (a fully qualified name under the zone). |  | MinLength: 1 <br /> |
| `type` _string_ | Type is the record type. |  | Enum: [A AAAA CNAME TXT MX NS SRV CAA] <br /> |
| `content` _string_ | Content is the record content (e.g. the IP for an A/AAAA record). |  | MinLength: 1 <br /> |
| `ttl` _integer_ | TTL is the record TTL in seconds. Defaults to 300. | 300 | Minimum: 1 <br />Optional: \{\} <br /> |


#### DNSRecordStatus



DNSRecordStatus defines the observed state of DNSRecord.



_Appears in:_
- [DNSRecord](#dnsrecord)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the last reconciled generation. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#condition-v1-meta) array_ | Conditions holds the conditions for the DNSRecord. |  | Optional: \{\} <br /> |
| `zoneID` _string_ | ZoneID is the id of the zone the record is created in. |  | Optional: \{\} <br /> |
| `recordID` _string_ | RecordID is the id of the created NetBird DNS record. |  | Optional: \{\} <br /> |


#### DNSZone



DNSZone is the Schema for the dnszones API. It is a thin mirror of a NetBird
managed DNS zone.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `netbird.io/v1alpha1` | | |
| `kind` _string_ | `DNSZone` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[DNSZoneSpec](#dnszonespec)_ |  |  | Required: \{\} <br /> |
| `status` _[DNSZoneStatus](#dnszonestatus)_ |  | \{ observedGeneration:-1 \} |  |


#### DNSZoneSpec



DNSZoneSpec defines the desired state of DNSZone. It mirrors the NetBird
DNS-zones API (POST /api/dns/zones) 1:1. The controller adopts an existing
zone with the same domain rather than failing, so a zone provisioned out of
band is taken over.



_Appears in:_
- [DNSZone](#dnszone)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the managed zone. |  | MinLength: 1 <br /> |
| `domain` _string_ | Domain is the zone apex, e.g. "kube.example.com". |  | MinLength: 1 <br /> |
| `distributionGroups` _[GroupReference](#groupreference) array_ | DistributionGroups are the NetBird groups whose peers receive the zone, so<br />they can resolve records in it. The reverse-proxy cluster that fronts a<br />service must be in one of these groups for hostname upstreams to resolve. |  | Optional: \{\} <br /> |
| `enableSearchDomain` _boolean_ | EnableSearchDomain adds the zone as a search domain on distributed peers. |  | Optional: \{\} <br /> |
| `enabled` _boolean_ | Enabled controls whether the zone is active. Defaults to true. | true | Optional: \{\} <br /> |


#### DNSZoneStatus



DNSZoneStatus defines the observed state of DNSZone.



_Appears in:_
- [DNSZone](#dnszone)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the last reconciled generation. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#condition-v1-meta) array_ | Conditions holds the conditions for the DNSZone. |  | Optional: \{\} <br /> |
| `zoneID` _string_ | ZoneID is the id of the managed NetBird zone. |  | Optional: \{\} <br /> |


#### Group



Group is the Schema for the groups API.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `netbird.io/v1alpha1` | | |
| `kind` _string_ | `Group` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[GroupSpec](#groupspec)_ |  |  | Required: \{\} <br /> |
| `status` _[GroupStatus](#groupstatus)_ |  | \{ observedGeneration:-1 \} |  |


#### GroupReference







_Appears in:_
- [ClusterProxySpec](#clusterproxyspec)
- [DNSZoneSpec](#dnszonespec)
- [NetworkResourceSpec](#networkresourcespec)
- [NetworkRouterPeers](#networkrouterpeers)
- [ReverseProxyServiceSpec](#reverseproxyservicespec)
- [SetupKeySpec](#setupkeyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the group. |  | Optional: \{\} <br /> |
| `id` _string_ | ID is the id of the group. |  | Optional: \{\} <br /> |
| `localRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#localobjectreference-v1-core)_ | LocalReference is a reference to a group in the same namespace. |  | Optional: \{\} <br /> |


#### GroupSpec



GroupSpec defines the desired state of Group.



_Appears in:_
- [Group](#group)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the group. |  | MinLength: 1 <br /> |


#### GroupStatus



GroupStatus defines the observed state of Group.



_Appears in:_
- [Group](#group)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the last reconciled generation. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#condition-v1-meta) array_ | Conditions holds the conditions for the Group. |  | Optional: \{\} <br /> |
| `groupID` _string_ | GroupID is the id of the created group. |  | Optional: \{\} <br /> |


#### InjectionMode

_Underlying type:_ _string_

InjectionMode defines how the sidecar is injected into the pod.

_Validation:_
- Enum: [Sidecar Container]

_Appears in:_
- [SidecarProfileSpec](#sidecarprofilespec)

| Field | Description |
| --- | --- |
| `Sidecar` | InjectionModeSidecar injects the client as a sidecar container.<br /> |
| `Container` | InjectionModeContainer injects the client as a regular container.<br /> |


#### Network



Network is the Schema for the networks API. It is a thin mirror of a NetBird
network.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `netbird.io/v1alpha1` | | |
| `kind` _string_ | `Network` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[NetworkSpec](#networkspec)_ |  |  | Required: \{\} <br /> |
| `status` _[NetworkStatus](#networkstatus)_ |  | \{ observedGeneration:-1 \} |  |


#### NetworkResource



NetworkResource is the Schema for the networkresources API. It is a thin
mirror of a NetBird network resource (one address).





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `netbird.io/v1alpha1` | | |
| `kind` _string_ | `NetworkResource` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[NetworkResourceSpec](#networkresourcespec)_ |  |  | Required: \{\} <br /> |
| `status` _[NetworkResourceStatus](#networkresourcestatus)_ |  | \{ observedGeneration:-1 \} |  |


#### NetworkResourceSpec



NetworkResourceSpec defines the desired state of NetworkResource. It mirrors
the NetBird network-resource API (POST /api/networks/{network}/resources) 1:1:
a single address routed into a network, with groups. DNS is handled
separately by DNSRecord; IP-family fan-out is done by the translation layer
(one NetworkResource per address family).



_Appears in:_
- [NetworkResource](#networkresource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `networkRef` _[CrossNamespaceReference](#crossnamespacereference)_ | NetworkRef references the Network this resource is created in. The Network<br />must be Ready; its status.networkID identifies the NetBird network. |  |  |
| `name` _string_ | Name of the resource. |  | MinLength: 1 <br /> |
| `address` _string_ | Address is the single resource address — an IP, CIDR, or domain. NetBird<br />derives the resource type from it. |  | MinLength: 1 <br /> |
| `description` _string_ | Description of the resource. |  | Optional: \{\} <br /> |
| `groups` _[GroupReference](#groupreference) array_ | Groups are the NetBird groups this resource is a part of, referenced by<br />name, id, or local Group reference. |  | Optional: \{\} <br /> |
| `enabled` _boolean_ | Enabled controls whether the resource is active. Defaults to true. | true | Optional: \{\} <br /> |


#### NetworkResourceStatus



NetworkResourceStatus defines the observed state of NetworkResource.



_Appears in:_
- [NetworkResource](#networkresource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the last reconciled generation. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#condition-v1-meta) array_ | Conditions holds the conditions for the NetworkResource. |  | Optional: \{\} <br /> |
| `networkID` _string_ | NetworkID is the id of the network the resource is created in. |  | Optional: \{\} <br /> |
| `resourceID` _string_ | ResourceID is the id of the created NetBird resource. |  | Optional: \{\} <br /> |


#### NetworkRouter



NetworkRouter is the Schema for the networkrouters API: a NetBird router (a
peer group bound to a network) plus its routing-peer source.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `netbird.io/v1alpha1` | | |
| `kind` _string_ | `NetworkRouter` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[NetworkRouterSpec](#networkrouterspec)_ |  |  | Required: \{\} <br /> |
| `status` _[NetworkRouterStatus](#networkrouterstatus)_ |  | \{ observedGeneration:-1 \} |  |


#### NetworkRouterPeers



NetworkRouterPeers selects the routing peers. Exactly one field must be set.



_Appears in:_
- [NetworkRouterSpec](#networkrouterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `group` _[GroupReference](#groupreference)_ | Group reuses an existing NetBird group as the routing peers (e.g. the group<br />the host-level netbird on the cluster nodes auto-joins). The operator<br />creates only the router and deploys nothing. |  | Optional: \{\} <br /> |
| `deploy` _[RouterDeploy](#routerdeploy)_ | Deploy runs a hostNetwork DaemonSet of netbird-client as the routing peers;<br />the operator manages its Group, SetupKey and DaemonSet. |  | Optional: \{\} <br /> |


#### NetworkRouterSpec



NetworkRouterSpec mirrors the NetBird router API
(POST /api/networks/{network}/routers) and adds the routing-peer source: an
existing NetBird group, or a netbird-client DaemonSet the operator deploys.



_Appears in:_
- [NetworkRouter](#networkrouter)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `networkRef` _[CrossNamespaceReference](#crossnamespacereference)_ | NetworkRef references the Network this router belongs to. The Network must<br />be Ready; its status.networkID identifies the NetBird network. |  |  |
| `peers` _[NetworkRouterPeers](#networkrouterpeers)_ | Peers selects the routing peers — exactly one of group or deploy. |  |  |
| `masquerade` _boolean_ | Masquerade makes the routing peers SNAT traffic to the routed resources. | true | Optional: \{\} <br /> |
| `metric` _integer_ | Metric is the route metric; the lowest number wins. | 9999 | Maximum: 9999 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `enabled` _boolean_ | Enabled controls whether the router is active. | true | Optional: \{\} <br /> |


#### NetworkRouterStatus



NetworkRouterStatus defines the observed state of NetworkRouter.



_Appears in:_
- [NetworkRouter](#networkrouter)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the last reconciled generation. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#condition-v1-meta) array_ | Conditions holds the conditions for the NetworkRouter. |  | Optional: \{\} <br /> |
| `networkID` _string_ | NetworkID is the id of the network the router is created in. |  | Optional: \{\} <br /> |
| `routerID` _string_ | RouterID is the id of the created NetBird router. |  | Optional: \{\} <br /> |
| `groupID` _string_ | GroupID is the id of the peer group bound to the router. |  | Optional: \{\} <br /> |


#### NetworkSpec



NetworkSpec defines the desired state of Network. It mirrors the NetBird
Networks API (POST /api/networks) 1:1.



_Appears in:_
- [Network](#network)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the NetBird network. |  | MinLength: 1 <br /> |
| `description` _string_ | Description of the network. |  | Optional: \{\} <br /> |


#### NetworkStatus



NetworkStatus defines the observed state of Network.



_Appears in:_
- [Network](#network)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the last reconciled generation. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#condition-v1-meta) array_ | Conditions holds the conditions for the Network. |  | Optional: \{\} <br /> |
| `networkID` _string_ | NetworkID is the id of the created NetBird network. |  | Optional: \{\} <br /> |


#### ReverseProxyBackend



ReverseProxyBackend names a LoadBalancer Service this service proxies to. The
Service must be advertised (have a DNSRecord); the proxy targets its dualstack
FQDN, so IPv4/IPv6 is transparent.



_Appears in:_
- [ReverseProxyServiceSpec](#reverseproxyservicespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `serviceRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#localobjectreference-v1-core)_ | ServiceRef names the LoadBalancer Service to proxy to, in the same<br />namespace as the ReverseProxyService. |  |  |
| `port` _integer_ | Port the proxy dials on the backend. Defaults to the Service's first port. |  | Optional: \{\} <br /> |
| `path` _string_ | Path is the URL path prefix this backend serves (HTTP). Defaults to "/". |  | Optional: \{\} <br /> |


#### ReverseProxyMode

_Underlying type:_ _string_

ReverseProxyMode selects the proxy mode. "http" is an L7 reverse proxy
(path-based routing, TLS terminated at the edge). "tcp"/"tls"/"udp" are L4
passthrough on a fixed ListenPort — used for non-HTTP backends such as mail
(SMTP/IMAP/ManageSieve), where the backend terminates TLS itself. Maps to the
NetBird API ServiceRequest.mode.

_Validation:_
- Enum: [http tcp tls udp]

_Appears in:_
- [ReverseProxyServiceSpec](#reverseproxyservicespec)

| Field | Description |
| --- | --- |
| `http` |  |
| `tcp` |  |
| `tls` |  |
| `udp` |  |


#### ReverseProxyService



ReverseProxyService publishes LoadBalancer Services through the NetBird
reverse proxy, internally or externally. It is the admin's expose-or-not
decision.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `netbird.io/v1alpha1` | | |
| `kind` _string_ | `ReverseProxyService` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ReverseProxyServiceSpec](#reverseproxyservicespec)_ |  |  | Required: \{\} <br /> |
| `status` _[ReverseProxyServiceStatus](#reverseproxyservicestatus)_ |  | \{ observedGeneration:-1 \} |  |


#### ReverseProxyServiceSpec



ReverseProxyServiceSpec defines the desired state of ReverseProxyService. It
is admin-authored — creating one is the explicit decision to expose Services
through the NetBird reverse proxy, internally or externally. It mirrors the
NetBird reverse-proxy service API (POST /api/reverse-proxies/services),
targeting the DNSRecord FQDN that belongs to each backend LoadBalancer Service.



_Appears in:_
- [ReverseProxyService](#reverseproxyservice)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `backends` _[ReverseProxyBackend](#reverseproxybackend) array_ | Backends are the LoadBalancer Services this service proxies to, by path. |  | MaxItems: 64 <br />MinItems: 1 <br /> |
| `mode` _[ReverseProxyMode](#reverseproxymode)_ | Mode selects the proxy mode. "http" (default) is an L7 reverse proxy;<br />"tcp"/"tls"/"udp" are L4 passthrough on ListenPort. Expose several L4 ports<br />under one hostname with one CR per port (same Domain, distinct ListenPort). | http | Enum: [http tcp tls udp] <br />Optional: \{\} <br /> |
| `listenPort` _integer_ | ListenPort is the port the proxy listens on (L4 modes only — tcp/tls/udp).<br />0 (or unset) lets NetBird auto-assign. Ignored for mode=http. |  | Maximum: 65535 <br />Minimum: 0 <br />Optional: \{\} <br /> |
| `proxyCluster` _string_ | ProxyCluster is the address of the NetBird reverse-proxy cluster that<br />serves this service, e.g. "gate.example.com". The operator resolves it to<br />a proxy-cluster ID and points the service's targets at it. |  | MinLength: 1 <br /> |
| `domain` _string_ | Domain is the hostname the service is published under. |  | MinLength: 1 <br /> |
| `private` _boolean_ | Private, when true, makes the service NetBird-only: inbound peers<br />authenticate via their tunnel identity (no OIDC) and an ACL policy is<br />auto-generated from AccessGroups. |  | Optional: \{\} <br /> |
| `accessGroups` _[GroupReference](#groupreference) array_ | AccessGroups are the NetBird groups whose peers may reach a private<br />service over the tunnel. Required when Private is true; ignored otherwise. |  | Optional: \{\} <br /> |
| `crowdsecMode` _[CrowdsecMode](#crowdsecmode)_ | CrowdsecMode sets the CrowdSec IP-reputation handling for the service. |  | Enum: [off observe enforce] <br />Optional: \{\} <br /> |
| `accessRestrictions` _[AccessRestrictions](#accessrestrictions)_ | AccessRestrictions sets IP/geo connection-level restrictions. |  | Optional: \{\} <br /> |
| `passHostHeader` _boolean_ | PassHostHeader, when true, forwards the original client Host header to the<br />backend instead of rewriting it to the backend address. |  | Optional: \{\} <br /> |
| `rewriteRedirects` _boolean_ | RewriteRedirects, when true, rewrites Location headers in backend<br />responses to replace the backend address with the public domain. |  | Optional: \{\} <br /> |


#### ReverseProxyServiceStatus



ReverseProxyServiceStatus defines the observed state of ReverseProxyService.



_Appears in:_
- [ReverseProxyService](#reverseproxyservice)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the last reconciled generation. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#condition-v1-meta) array_ | Conditions holds the conditions for the ReverseProxyService. |  | Optional: \{\} <br /> |
| `serviceID` _string_ | ServiceID is the id of the created NetBird reverse-proxy service. |  | Optional: \{\} <br /> |


#### RouterDeploy



RouterDeploy configures the netbird-client DaemonSet for peers.deploy.



_Appears in:_
- [NetworkRouterPeers](#networkrouterpeers)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeSelector` _object (keys:string, values:string)_ | NodeSelector limits the DaemonSet to matching nodes (default: all nodes). |  | Optional: \{\} <br /> |
| `image` _string_ | Image overrides the netbird-client image (default: the operator's<br />configured client image). |  | Optional: \{\} <br /> |
| `logLevel` _string_ | LogLevel for the netbird client. |  | Enum: [error warn info debug trace] <br />Optional: \{\} <br /> |


#### SetupKey



SetupKey is the Schema for the setupkeys API.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `netbird.io/v1alpha1` | | |
| `kind` _string_ | `SetupKey` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SetupKeySpec](#setupkeyspec)_ |  |  | Required: \{\} <br /> |
| `status` _[SetupKeyStatus](#setupkeystatus)_ |  | \{ observedGeneration:-1 \} |  |


#### SetupKeySpec



SetupKeySpec defines the desired state of SetupKey.



_Appears in:_
- [SetupKey](#setupkey)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the setup key. |  | MinLength: 1 <br /> |
| `ephemeral` _boolean_ | Ephemeral decides if peers added with the key are ephemeral or not. |  |  |
| `allowExtraDnsLabels` _boolean_ | AllowExtraDnsLabels decides if peers added with the key can have extra DNS labels. | false |  |
| `duration` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#duration-v1-meta)_ | Duration sets how long the setup key is valid for. When unset (or zero) the<br />key does not expire — set a duration for time-limited keys. Only minute (m)<br />and hour (h) units are accepted. |  | Pattern: `^([0-9]+(\.[0-9]+)?(m\|h))+$` <br />Type: string <br />Optional: \{\} <br /> |
| `autoGroups` _[GroupReference](#groupreference) array_ | AutoGroups are groups that will be automatically assigned to peers using setup key. |  | Optional: \{\} <br /> |


#### SetupKeyStatus



SetupKeyStatus defines the observed state of SetupKey.



_Appears in:_
- [SetupKey](#setupkey)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the last reconciled generation. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#condition-v1-meta) array_ | Conditions holds the conditions for the SetupKey. |  | Optional: \{\} <br /> |
| `setupKeyID` _string_ | SetupKeyID is the id of the created setup key. |  |  |


#### SidecarProfile



SidecarProfile is the Schema for the sidecarprofiles API.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `netbird.io/v1alpha1` | | |
| `kind` _string_ | `SidecarProfile` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SidecarProfileSpec](#sidecarprofilespec)_ |  |  | Required: \{\} <br /> |
| `status` _[SidecarProfileStatus](#sidecarprofilestatus)_ |  | \{  \} |  |


#### SidecarProfileSpec



SidecarProfileSpec defines the desired state of SidecarProfile.



_Appears in:_
- [SidecarProfile](#sidecarprofile)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `setupKeyRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#localobjectreference-v1-core)_ | SetupKeyRef is the reference to the setup key used in the client. |  | Required: \{\} <br /> |
| `podSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#labelselector-v1-meta)_ | PodSelector determines which pods the profile should apply to.<br />An empty slector means the profile will apply to all pods in the namespace. |  | Optional: \{\} <br /> |
| `injectionMode` _[InjectionMode](#injectionmode)_ | InjectionMode defines whether the sidecar is injected as a native Kubernetes sidecar container or as a regular container. | Sidecar | Enum: [Sidecar Container] <br />Optional: \{\} <br /> |
| `extraDNSLabels` _string array_ | ExtraDNSLabels assigns additional DNS names to peers beyond their default hostname. |  | Optional: \{\} <br /> |
| `containerOverride` _[ContainerOverride](#containeroverride)_ |  |  | Optional: \{\} <br /> |


#### SidecarProfileStatus



SidecarProfileStatus defines the observed state of SidecarProfile.



_Appears in:_
- [SidecarProfile](#sidecarprofile)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the last reconciled generation. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.35/#condition-v1-meta) array_ | Conditions holds the conditions for the SidecarProfile. |  | Optional: \{\} <br /> |


