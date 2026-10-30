# Licensing

This repository contains source files under different licenses. The applicable license follows each file's origin and notices; there is no repository-wide choice between MPL and AGPL.

## Effective Commit and Inherited Baseline

This policy takes effect with the commit that first adds this document. The inherited baseline is upstream commit [`0a29338e1f102a462363ce3527417029f89bab28`](https://github.com/cedar2025/xboard-node/commit/0a29338e1f102a462363ce3527417029f89bab28).

The policy preserves the MPL-2.0 licensing of inherited source and does not change the licenses of earlier versions. A file's creation date or its first appearance in this branch does not, by itself, determine its license.

## File-Level Policy

| Material | License |
| --- | --- |
| Inherited MPL-covered source files, including edits and additions to those files | MPL-2.0 |
| New files containing copied, moved, or adapted MPL-covered source | MPL-2.0 |
| Independently authored new source files introduced with this policy or later, containing no inherited MPL-covered source | AGPL-3.0-only |
| Project documentation and configuration without a more specific notice | MPL-2.0 |
| Third-party code and assets | Their existing licenses and notices |

The initial independently authored AGPL-3.0-only files are:

- `internal/kernel/xray/bandwidth.go`
- `internal/kernel/xray/bandwidth_test.go`
- `internal/kernel/integration_test.go`

The initial AGPL-only files listed above carry explicit SPDX identifiers; inherited source files may not yet have a header. A missing header does not change a file's license under this policy. Preserve existing copyright and attribution notices. New independent source files must use `SPDX-License-Identifier: AGPL-3.0-only`; edits to inherited MPL files must remain available under MPL-2.0. Moving inherited code into a new file does not make it eligible for AGPL-only licensing. The Compose configuration, adapted from the upstream `compose` branch, remains MPL-2.0.

`AGPL-3.0-only` means version 3 only; this policy does not grant an option to use later AGPL versions.

## Combined Programs and Source Availability

When MPL-covered source is distributed as part of the combined AGPL-covered program, it is additionally available under AGPL-3.0-only to the extent permitted by [MPL-2.0 section 3.3](https://www.mozilla.org/en-US/MPL/2.0/#distribution-of-a-larger-work). Its MPL-2.0 rights and notices remain intact. This secondary-licensing provision does not offer an MPL option for the independent AGPL-only files and does not apply to code marked incompatible with secondary licenses.

Distributions must satisfy the applicable source-availability and notice requirements of both licenses. Under AGPL section 13, operators of modified versions that support remote network interaction must offer those users access to the Corresponding Source, as defined by the license. The combined program's obligations are not limited to the newly added lines of code.

Source for this fork is available at [kirakirai8023/Xboard-Node](https://github.com/kirakirai8023/Xboard-Node). Use the tag or commit corresponding to a binary release when obtaining its source. The release workflow packages the project license notices as an archive, and container builds copy them to `/usr/share/licenses/xboard-node/`.

## License Texts and Dependencies

- [Mozilla Public License 2.0](LICENSES/MPL-2.0.txt)
- [GNU Affero General Public License, version 3 only](LICENSES/AGPL-3.0-only.txt)

The license texts are reproduced verbatim from the [SPDX License List](https://spdx.org/licenses/).

Dependencies retain their own licenses. In particular, [sing-box v1.14.4](https://github.com/SagerNet/sing-box/blob/v1.14.4/LICENSE) uses GPL-3.0-or-later, and [Xray](https://github.com/XTLS/Xray-core/blob/0afec66085a85191e9dd2a4a6646c7c9a94688d1/LICENSE) uses MPL-2.0. This file does not replace third-party license terms or the copyright notices required for distributions that include those dependencies.
