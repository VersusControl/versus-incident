package middleware

import "testing"

func TestGatewayAuthPolicy(t *testing.T) {
	SetGatewayAuthEnabled(true)
	t.Cleanup(func() { SetGatewayAuthEnabled(true) })
	if !GatewayAuthEnabled() {
		t.Fatal("GatewayAuthEnabled() = false, want true")
	}
	SetGatewayAuthEnabled(false)
	if GatewayAuthEnabled() {
		t.Fatal("GatewayAuthEnabled() = true, want false")
	}
}
