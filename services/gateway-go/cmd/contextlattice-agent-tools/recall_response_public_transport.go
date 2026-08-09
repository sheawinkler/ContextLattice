package main

import "net/http"

// The OSS recall-response transport has no paid entitlement or machine-binding
// headers. The dedicated CLI keeps the same bounded transport seam in every
// lane while the paid lane supplies its signed runtime-license implementation.
func entitlementHeaders() (map[string]string, error) {
	return map[string]string{}, nil
}

func addRuntimeLicenseRequestProof(_ *http.Request, _ []byte) error {
	return nil
}
