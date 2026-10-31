// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"fmt"
	"strings"
)

const (
	CertSourcePanel = "panel"
	CertSourceLocal = "local"
)

// UsesLocalSource reports an explicit administrator choice to manage this
// instance's certificates locally, including when the panel sends cert_mode=none.
func (c CertConfig) UsesLocalSource() bool {
	return strings.EqualFold(strings.TrimSpace(c.Source), CertSourceLocal)
}

// ValidateSource rejects unknown certificate policy owners without echoing input.
func (c CertConfig) ValidateSource() error {
	switch strings.ToLower(strings.TrimSpace(c.Source)) {
	case "", CertSourcePanel, CertSourceLocal:
		return nil
	default:
		return fmt.Errorf("cert.source must be 'panel' or 'local'")
	}
}

// ResolveCertificatePolicy shares the certificate precedence rule between
// deployment inspection and daemon startup/updates. The bool identifies a
// selected panel policy; an absent panel policy retains the local fallback.
func ResolveCertificatePolicy(local CertConfig, panel *CertConfig) (CertConfig, bool) {
	if local.UsesLocalSource() || panel == nil {
		return local, false
	}
	return *panel, true
}
