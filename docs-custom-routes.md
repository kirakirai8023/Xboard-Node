# Custom Routes

`custom_route_rules` and `custom_routes` are fields in the panel's node JSON. For local kernel-native rules, use `kernel.custom_route` or the file referenced by `kernel.custom_config`. Standalone YAML supports structured rules at `standalone.node.custom_route_rules`.

## Example

```json
{
  "custom_route_rules": [
    {
      "name": "direct-example",
      "match": {"domain_suffixes": ["example.com"]},
      "action": {"type": "direct"}
    },
    {
      "name": "block-ads",
      "match": {"domain_suffixes": ["ads.example.net"]},
      "action": {"type": "block"}
    },
    {
      "name": "route-proxy",
      "match": {"ip_cidrs": ["1.1.1.0/24"]},
      "action": {"type": "route", "target": "us-proxy"}
    }
  ]
}
```

Define `us-proxy` as an outbound before using this example. Rules are evaluated in order; the first matching native rule wins. Put more specific rules before broader ones.

## Match Semantics

**Different populated match fields are alternatives, not combined constraints.** The compiler emits a separate native rule for each field. For example, `ip_cidrs: ["1.1.1.0/24"]` together with `ports: ["443"]` matches that CIDR on any port **or** port 443 at any destination. Use a single kernel-native rule when all conditions must match together. Values within each list are also alternatives.

| Field | Meaning | Example |
| --- | --- | --- |
| `domains` | Kernel-native domain entries: exact names in sing-box; Xray receives entries unchanged, so bare names use Xray's substring matching | `["api.example.com"]` |
| `domain_suffixes` | Domain and subdomain suffixes | `["example.com"]` |
| `ip_cidrs` | Destination IP ranges | `["203.0.113.0/24"]` |
| `ports` | Destination ports or ranges | `["443", "8000-9000"]` |
| `networks` | TCP or UDP transport | `["tcp"]` or `["udp"]` |
| `source_cidrs` | Source IP ranges | `["192.0.2.0/24"]` |
| `source_ports` | Source ports or ranges | `["1024-65535"]` |

Both kernels expose these fields, but native syntax is not interchangeable. For example, Xray's `full:` and `geosite:` domain prefixes are not translated into sing-box matchers. A rule may set `disabled: true` to skip it. Active rules require at least one match condition and a supported action.

## Actions

| Action | Effect |
| --- | --- |
| `{"type": "direct"}` | Select the outbound tagged `direct` |
| `{"type": "block"}` | Select the outbound tagged `block` |
| `{"type": "route", "target": "tag"}` | Select an existing outbound by tag |

Tags must match the actual kernel configuration, including case. Local configuration can replace the `direct` and `block` outbounds, changing their behavior.

## Application Order

1. Rules from `kernel.custom_config`: `routing.rules` for Xray or `route.rules` for sing-box.
2. Structured panel `custom_route_rules`.
3. Raw panel `custom_routes`, then local `kernel.custom_route`.
4. Built-in private-address block rules.
5. Legacy panel `routes`.

Custom rules can take precedence over private-address blocking. Review broad direct/proxy rules carefully. The built-in CIDR rules are not a complete host-isolation boundary, especially for domain-based destinations; enforce host-network restrictions with the kernel's native controls or a firewall when needed.

Use structured rules for the shared field set. Use native rules for compound matches or kernel-specific features such as load balancing. Do not assume a native rule for one kernel works with the other.
