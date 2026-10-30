package model

import "testing"

// A thumbprint copied from the Windows certificate dialog starts with an invisible left-to-right mark (U+200E) and
// has spaces; Validate must strip both.
func TestCloudThumbprintCopiedFromTheCertificateDialog(t *testing.T) {
	c := CloudSettings{CertThumbprint: "\u200ea1 b2 c3 d4 e5 f6 a7 b8 c9 d0 e1 f2 a3 b4 c5 d6 e7 f8 a9 b0"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if want := "A1B2C3D4E5F6A7B8C9D0E1F2A3B4C5D6E7F8A9B0"; c.CertThumbprint != want {
		t.Fatalf("thumbprint %q, want %q", c.CertThumbprint, want)
	}
}
