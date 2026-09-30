package cloudwatchlogs

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
)

const (
	MaxPages       = 3
	MaxRequests    = 3
	MaxRows        = 50
	MaxFields      = 40
	MaxBytes       = 32 * 1024
	MaxLookback    = 6 * time.Hour
	Timeout        = 8 * time.Second
	pageSize       = 100
	maxScannedRows = MaxPages * pageSize
	maxEventBytes  = 128 * 1024
	maxDataBytes   = MaxBytes - 1024
)

var ErrScope = errors.New("invalid CloudWatch Logs read scope or arguments")
var ErrResponse = errors.New("CloudWatch Logs read response exceeded its bound")

type API interface {
	FilterLogEvents(context.Context, *cloudwatchlogs.FilterLogEventsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error)
}

type Scope struct {
	Region, LogGroupName, LogStreamPrefix, FilterPattern string
}

type Request struct {
	Search          string
	LookbackMinutes int
	Limit           int
}

type Record struct {
	Timestamp string `json:"timestamp"`
	Stream    string `json:"stream"`
	Message   string `json:"message"`
}

type Result struct {
	Count      int      `json:"count"`
	Records    []Record `json:"records,omitempty"`
	Fields     []string `json:"fields,omitempty"`
	Sampled    bool     `json:"sampled"`
	Truncated  bool     `json:"truncated"`
	Truncation []string `json:"truncation,omitempty"`
}

type Reader struct {
	api      API
	scope    Scope
	scrubber core.Scrubber
	now      func() time.Time
}

func NewReader(api API, scope Scope, scrubber core.Scrubber) (*Reader, error) {
	if api == nil || !regionPattern.MatchString(scope.Region) || scope.LogGroupName == "" || len(scope.LogGroupName) > 512 ||
		len(scope.LogStreamPrefix) > 512 || len(scope.FilterPattern) > 1024 || strings.TrimSpace(scope.Region) != scope.Region ||
		strings.TrimSpace(scope.LogGroupName) != scope.LogGroupName || strings.ContainsAny(scope.LogGroupName+scope.LogStreamPrefix+scope.FilterPattern, "\x00\r\n") {
		return nil, ErrScope
	}
	return &Reader{api: api, scope: scope, scrubber: scrubber, now: time.Now}, nil
}

var (
	arnPattern     = regexp.MustCompile(`arn:[^\s,"'<>]+`)
	accountPattern = regexp.MustCompile(`\b[0-9]{12}\b`)
	ipv4Pattern    = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
)

func (reader *Reader) Safe(value string) string {
	if reader.scrubber != nil {
		value = reader.scrubber.Scrub(value)
	}
	value = arnPattern.ReplaceAllString(value, "[redacted]")
	value = accountPattern.ReplaceAllString(value, "[redacted]")
	value = redactIPv6(value)
	value = ipv4Pattern.ReplaceAllString(value, "[redacted]")
	return value
}

func ipv6Hex(byteValue byte) bool {
	return byteValue >= '0' && byteValue <= '9' || byteValue >= 'a' && byteValue <= 'f' || byteValue >= 'A' && byteValue <= 'F'
}

func ipv6Address(candidate string) bool {
	address, err := netip.ParseAddr(candidate)
	return err == nil && address.Is6() && candidate != "::"
}

func redactIPv6(value string) string {
	var output strings.Builder
	output.Grow(len(value))
	for position := 0; position < len(value); {
		if !ipv6Hex(value[position]) && value[position] != ':' || position > 0 && (ipv6Hex(value[position-1]) || value[position-1] >= 'g' && value[position-1] <= 'z' || value[position-1] >= 'G' && value[position-1] <= 'Z') {
			output.WriteByte(value[position])
			position++
			continue
		}
		end := position
		for end < len(value) && (ipv6Hex(value[end]) || value[end] == ':' || value[end] == '.' || value[end] >= '0' && value[end] <= '9') {
			end++
		}
		if end == position {
			output.WriteByte(value[position])
			position++
			continue
		}
		part := value[position:end]
		if len(part) > 128 && strings.Count(part, ":") >= 2 {
			output.WriteString("[redacted]")
			position = end
			continue
		}
		matchEnd := 0
		if len(part) <= 128 {
			for prefixEnd := len(part); prefixEnd > 0; prefixEnd-- {
				if ipv6Address(part[:prefixEnd]) && (prefixEnd == len(part) || strings.Trim(part[prefixEnd:], ":.") == "" ||
					(part[prefixEnd] == '.' || part[prefixEnd] == ':') && prefixEnd+1 < len(part) && part[prefixEnd+1] != part[prefixEnd] && !hasIPv6Prefix(part[prefixEnd+1:])) {
					matchEnd = prefixEnd
					break
				}
			}
			if matchEnd == 0 {
				for split := 1; split < len(part)-1; split++ {
					if (part[split] == ':' || part[split] == '.') && ipv6Address(part[:split]) && hasIPv6Prefix(part[split+1:]) {
						matchEnd = split
						break
					}
				}
			}
		}
		if matchEnd > 0 && position+matchEnd < len(value) && strings.Count(part[:matchEnd], ":") == 2 &&
			(value[position+matchEnd] >= 'a' && value[position+matchEnd] <= 'z' || value[position+matchEnd] >= 'A' && value[position+matchEnd] <= 'Z') {
			matchEnd = 0
		}
		if matchEnd == 0 {
			output.WriteString(part)
			position = end
			continue
		}
		zoneEnd := position + matchEnd
		if zoneEnd < len(value) && value[zoneEnd] == '%' {
			zoneEnd++
			for zoneEnd < len(value) {
				character := value[zoneEnd]
				if character == '-' || character == '_' {
					if next := zoneEnd + 1; next < len(value) && ipv6Hex(value[next]) && hasIPv6At(value, next) {
						break
					}
				}
				if character != '-' && character != '_' && !ipv6Hex(character) && (character < 'g' || character > 'z') && (character < 'G' || character > 'Z') && (character < '0' || character > '9') {
					break
				}
				zoneEnd++
			}
			if zoneEnd == position+matchEnd+1 {
				zoneEnd--
			}
		}
		output.WriteString("[redacted]")
		position += matchEnd
		if zoneEnd > position {
			position = zoneEnd
		}
	}
	return output.String()
}

func hasIPv6Prefix(value string) bool {
	for end := len(value); end > 0; end-- {
		if ipv6Address(value[:end]) {
			return true
		}
	}
	return false
}

func hasIPv6At(value string, start int) bool {
	end := start
	for end < len(value) && end-start <= 128 && (ipv6Hex(value[end]) || value[end] == ':' || value[end] == '.') {
		end++
	}
	return end-start <= 128 && ipv6Address(value[start:end])
}

func (reader *Reader) Read(ctx context.Context, request Request) (Result, error) {
	return reader.scan(ctx, request, false)
}

func (reader *Reader) Discover(ctx context.Context, request Request) (Result, error) {
	return reader.scan(ctx, request, true)
}

func (reader *Reader) scan(ctx context.Context, request Request, discovery bool) (Result, error) {
	result := Result{Sampled: discovery}
	if request.Limit == 0 {
		request.Limit = 20
	}
	if request.LookbackMinutes == 0 {
		request.LookbackMinutes = 60
	}
	if request.Limit < 1 || request.Limit > MaxRows || request.LookbackMinutes < 1 ||
		time.Duration(request.LookbackMinutes)*time.Minute > MaxLookback || len(request.Search) > 256 ||
		strings.ContainsAny(request.Search, "\r\n\x00") {
		return result, ErrScope
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	end := reader.now().UTC().UnixMilli()
	start := end - int64(request.LookbackMinutes)*int64(time.Minute/time.Millisecond)
	limit := int32(pageSize)
	input := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: &reader.scope.LogGroupName, LogStreamNamePrefix: nil,
		StartTime: &start, EndTime: &end, Limit: &limit,
	}
	if reader.scope.LogStreamPrefix != "" {
		input.LogStreamNamePrefix = &reader.scope.LogStreamPrefix
	}
	if reader.scope.FilterPattern != "" {
		input.FilterPattern = &reader.scope.FilterPattern
	}
	fields := map[string]bool{}
	seenTokens := map[string]bool{}
	usedBytes := 0
	scanned := 0
	for page := 0; page < MaxPages && page < MaxRequests; page++ {
		if err := ctx.Err(); err != nil {
			if page > 0 {
				result.Truncated, result.Truncation = true, []string{"response_limit"}
				break
			}
			return Result{}, err
		}
		response, err := reader.api.FilterLogEvents(ctx, input)
		if err != nil {
			if page > 0 {
				result.Truncated, result.Truncation = true, []string{"response_limit"}
				break
			}
			return Result{}, err
		}
		if err := ctx.Err(); err != nil {
			if page > 0 {
				result.Truncated, result.Truncation = true, []string{"response_limit"}
				break
			}
			return Result{}, err
		}
		if response == nil || len(response.Events) > pageSize {
			if page > 0 {
				result.Truncated, result.Truncation = true, []string{"response_limit"}
				break
			}
			return Result{}, ErrResponse
		}
		for _, event := range response.Events {
			scanned++
			if scanned > maxScannedRows {
				result.Truncated, result.Truncation = true, []string{"scan_limit"}
				break
			}
			if event.Message == nil || event.Timestamp == nil || event.LogStreamName == nil ||
				(reader.scope.LogStreamPrefix != "" && !strings.HasPrefix(*event.LogStreamName, reader.scope.LogStreamPrefix)) ||
				*event.Timestamp < start || *event.Timestamp > end {
				continue
			}
			if len(*event.Message)+len(*event.LogStreamName) > maxEventBytes {
				result.Truncated, result.Truncation = true, []string{"byte_limit"}
				break
			}
			if discovery {
				var object map[string]json.RawMessage
				if json.Unmarshal([]byte(*event.Message), &object) != nil {
					continue
				}
				for key := range object {
					key = reader.Safe(key)
					if key == "" || fields[key] {
						continue
					}
					encodedKey, _ := json.Marshal(key)
					if len(fields) >= MaxFields || usedBytes+len(encodedKey)+1 > maxDataBytes {
						result.Truncated, result.Truncation = true, []string{"field_or_byte_limit"}
						break
					}
					fields[key] = true
					usedBytes += len(encodedKey) + 1
				}
				result.Count++
				if result.Truncated {
					break
				}
			} else {
				message := reader.Safe(*event.Message)
				if request.Search != "" && !strings.Contains(message, request.Search) {
					continue
				}
				record := Record{Timestamp: time.UnixMilli(*event.Timestamp).UTC().Format(time.RFC3339Nano), Stream: reader.Safe(*event.LogStreamName), Message: message}
				encoded, _ := json.Marshal(record)
				if usedBytes+len(encoded)+2 > maxDataBytes {
					result.Truncated, result.Truncation = true, []string{"byte_limit"}
					break
				}
				usedBytes += len(encoded) + 2
				result.Records = append(result.Records, record)
				result.Count++
			}
			if result.Count >= request.Limit {
				result.Truncated, result.Truncation = true, []string{"result_limit"}
				break
			}
		}
		if result.Truncated {
			break
		}
		if response.NextToken == nil || *response.NextToken == "" {
			break
		}
		if seenTokens[*response.NextToken] {
			result.Truncated, result.Truncation = true, []string{"pagination_cycle"}
			break
		}
		seenTokens[*response.NextToken] = true
		if page+1 >= MaxPages || page+1 >= MaxRequests {
			result.Truncated, result.Truncation = true, []string{"page_limit"}
			break
		}
		input.NextToken = response.NextToken
	}
	for key := range fields {
		result.Fields = append(result.Fields, key)
	}
	sort.Strings(result.Fields)
	return result, nil
}
