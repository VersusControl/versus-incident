package awsauth

import (
	"errors"
	"net"
	"strings"

	"github.com/aws/smithy-go"
)

// ServiceNotInRegion reports a service that has no endpoint in the region
// it was called in: the regional host does not exist in DNS, or AWS answers
// that the service or operation is not offered there. That is not a
// network failure; there is nothing to read in that region. Provider text
// is only matched, never kept. A resolver that cannot answer (a temporary
// DNS failure) is not a missing host.
func ServiceNotInRegion(err error) bool {
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) && dnsError.IsNotFound {
		return true
	}
	var apiError smithy.APIError
	if !errors.As(err, &apiError) {
		return false
	}
	switch apiError.ErrorCode() {
	case "UnsupportedAPIEndpointException", "RegionNotSupportedException":
		return true
	case "UnsupportedOperation", "InvalidAction":
		return strings.Contains(strings.ToLower(apiError.ErrorMessage()), "region")
	}
	return false
}
