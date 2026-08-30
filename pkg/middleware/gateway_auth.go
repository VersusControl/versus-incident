package middleware

import "sync/atomic"

var gatewayAuthEnabled atomic.Bool

func init() {
	gatewayAuthEnabled.Store(true)
}

// SetGatewayAuthEnabled controls whether the OSS gateway secret may authorize
// requests. Enterprise boot disables it before mounting routes so only an
// upstream authorization middleware can admit protected requests.
func SetGatewayAuthEnabled(enabled bool) {
	gatewayAuthEnabled.Store(enabled)
}

// GatewayAuthEnabled reports whether gateway-secret exchange, header, and
// cookie authentication are enabled for this process.
func GatewayAuthEnabled() bool {
	return gatewayAuthEnabled.Load()
}
