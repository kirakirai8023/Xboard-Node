# DNS Providers

ACME DNS-01 uses `cert_mode: "dns"`. In local `config.yml`, certificate settings belong under **`cert`**, or `instances[].cert` for an instance:

```yaml
cert:
  cert_mode: "dns"
  domain: "node.example.com"
  email: "admin@example.com"
  dns_provider: "cloudflare"
  dns_env:
    CLOUDFLARE_DNS_API_TOKEN: "YOUR_DNS_API_TOKEN"
```

Panel node JSON uses `cert_config` for the same fields. A top-level `cert_config:` block is not the local YAML configuration key. DNS-01 does not require an inbound HTTP-01 listener on port 80; it requires control of the domain's DNS records.

## Provider Credentials

Provider names and aliases are case-insensitive. Keys in `dns_env` are case-sensitive. Most providers read this map directly; setting a process environment variable alone does not populate it. AWS, Azure, and Google providers can additionally use their SDK credential chains as described below.

| Provider | Aliases | Credentials and options in `dns_env` |
| --- | --- | --- |
| cloudflare | cf | `CLOUDFLARE_DNS_API_TOKEN`; alternatives: `CF_API_TOKEN`, `CLOUDFLARE_API_TOKEN` |
| alidns | aliyun | `ALICLOUD_ACCESS_KEY_ID`, `ALICLOUD_ACCESS_KEY_SECRET` |
| tencentcloud | tencent | `TENCENTCLOUD_SECRET_ID`, `TENCENTCLOUD_SECRET_KEY` |
| route53 | aws | Optional `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_REGION` (or `AWS_DEFAULT_REGION`), `AWS_PROFILE`, `AWS_HOSTED_ZONE_ID`; otherwise use the AWS credential chain |
| godaddy | | `GODADDY_API_TOKEN` as `KEY:SECRET`, or both `GODADDY_API_KEY` and `GODADDY_API_SECRET` |
| namecheap | | `NAMECHEAP_API_KEY`, `NAMECHEAP_API_USER` (or `NAMECHEAP_USER`); optional `NAMECHEAP_CLIENT_IP`, `NAMECHEAP_API_ENDPOINT` |
| namesilo | | `NAMESILO_API_TOKEN` |
| digitalocean | do | `DO_AUTH_TOKEN`; alternatives: `DIGITALOCEAN_TOKEN`, `DIGITALOCEAN_API_TOKEN` |
| linode | | `LINODE_TOKEN` |
| hetzner | | `HETZNER_API_TOKEN` |
| gandi | | `GANDI_BEARER_TOKEN`; alternatives: `GANDI_API_TOKEN`, `GANDI_PERSONAL_ACCESS_TOKEN` |
| porkbun | | `PORKBUN_API_KEY`, `PORKBUN_API_SECRET_KEY` |
| netlify | | `NETLIFY_PERSONAL_ACCESS_TOKEN` |
| azure | | Required `AZURE_SUBSCRIPTION_ID`, `AZURE_RESOURCE_GROUP_NAME` (or `AZURE_RESOURCE_GROUP`); optional `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`, otherwise use the Azure credential chain |
| googleclouddns | gcp, gcloud | Required `GCE_PROJECT` (or `GOOGLE_PROJECT_ID`, `GCP_PROJECT`); optional `GCE_SERVICE_ACCOUNT_FILE` (or `GOOGLE_APPLICATION_CREDENTIALS`), otherwise use Google default credentials |
| huaweicloud | huawei | `HUAWEICLOUD_ACCESS_KEY_ID`, `HUAWEICLOUD_SECRET_ACCESS_KEY`; optional `HUAWEICLOUD_REGION_ID`. Alternatives: `HUAWEI_ACCESS_KEY_ID`, `HUAWEI_SECRET_ACCESS_KEY`, `HUAWEI_REGION` |
| bunny | | `BUNNY_ACCESS_KEY` |
| duckdns | | `DUCKDNS_TOKEN` |
| ovh | | `OVH_APPLICATION_KEY`, `OVH_APPLICATION_SECRET`, `OVH_CONSUMER_KEY`; optional `OVH_ENDPOINT` |
| vultr | | `VULTR_API_TOKEN` |
| desec | | `DESEC_TOKEN` |

These fields reflect the builders in `internal/cert/dnsproviders/`. When alternatives are supplied, the first nonblank value in the builder's lookup order wins. Keep DNS credentials out of commits and restrict access to configuration files. Replace all example domains and credentials before requesting a certificate.
