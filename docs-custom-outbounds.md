# Custom Outbounds

`custom_outbounds` is a field in the panel's node JSON. Its envelope is shared, but `settings` must use the selected kernel's native format. A single settings object is not portable between Xray and sing-box.

## Examples

Xray SOCKS outbound:

```json
{
  "custom_outbounds": [
    {
      "tag": "us-proxy",
      "protocol": "socks",
      "settings": {"servers": [{"address": "proxy.example.com", "port": 1080}]}
    }
  ]
}
```

sing-box SOCKS outbound:

```json
{
  "custom_outbounds": [
    {
      "tag": "us-proxy",
      "protocol": "socks",
      "settings": {"server": "proxy.example.com", "server_port": 1080, "version": "5"}
    }
  ]
}
```

Use a real proxy address and its required authentication settings. Reference `us-proxy` from a route action to send traffic through it; see [Custom Routes](docs-custom-routes.md).

## Protocol Availability

| Protocol | Xray | sing-box |
| --- | --- | --- |
| vmess, vless, trojan, shadowsocks, socks, http | Supported | Supported |
| wireguard | Native WireGuard outbound | Legacy outbound removed; use a native endpoint instead |
| tuic, hysteria2 | Not exposed by this adapter | Requires `with_quic` |
| anytls | Not exposed by this adapter | Supported |
| naive | Not exposed by this adapter | Requires `with_naive_outbound`, absent from the default Makefile/Docker builds |
| mieru | Not supported | Not supported |

The adapter's validation list still accepts `wireguard` for sing-box, but official sing-box 1.14.4 rejects the legacy outbound at runtime. Configure WireGuard through `endpoints` in a native file referenced by `kernel.custom_config`, using a build with `with_wireguard`. Endpoint tags are not currently collected by structured route-target validation; reference them from native routes in that file.

The default Makefile and Docker builds include `with_quic` and `with_wireguard`. A successful structural validation does not prove that a protocol is included in a particular binary or that its native settings are valid.

## Fields and Chaining

| Field | Required | Meaning |
| --- | --- | --- |
| `tag` | Yes | Unique outbound tag referenced by routes |
| `protocol` | Yes | Protocol name accepted for the selected kernel |
| `settings` | Yes | Nonempty kernel-native settings object |
| `proxy_tag` | No | Xray chaining target, emitted as `proxySettings.tag` |

For Xray, `settings` remains nested under the outbound's `settings` key. For sing-box, its keys are flattened alongside `type` and `tag`.

The current sing-box adapter emits `proxy_tag` unchanged, which official sing-box does not accept as a dialer option. For sing-box chaining, omit `proxy_tag` and use native `detour` in `settings`, or configure the outbounds in a native file. `detour` references are checked by the kernel rather than this adapter's `proxy_tag` validator.

Tags must be unique across panel outbounds, `kernel.custom_outbound`, and native-file outbounds. Reuse exact tag spelling in references. An explicit `direct` or `block` outbound replaces the corresponding generated default.

Use `kernel.custom_outbound` for raw kernel-native outbound objects in local YAML. Use `kernel.custom_config` for a native JSON/YAML file when full outbound fields, endpoints, or other native configuration are needed. Neither path makes settings portable across kernels.
