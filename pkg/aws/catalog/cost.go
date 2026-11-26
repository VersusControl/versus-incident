package catalog

// What reading an account costs. Versus's reads are almost never billed by
// the service they call; the cost comes from what each call triggers in the
// customer's account: every call is a CloudTrail management event, and the
// features the customer already chose (GuardDuty, a second trail, a log
// pipeline) price events. A few APIs also bill per request. Prices are AWS
// list prices for us-east-1.
const (
	// PriceS3RequestUSD is one S3 GET-class request ($0.0004 per 1,000).
	PriceS3RequestUSD = 0.0004 / 1000
	// PriceKMSRequestUSD is one KMS request ($0.03 per 10,000).
	PriceKMSRequestUSD = 0.03 / 10000
	// PriceSQSRequestUSD is one SQS request ($0.40 per million).
	PriceSQSRequestUSD = 0.40 / 1000000
	// PriceGuardDutyEventUSD is one management event GuardDuty analyses
	// ($4.00 per million).
	PriceGuardDutyEventUSD = 4.0 / 1000000
	// PriceTrailCopyEventUSD is one management event recorded by a trail
	// after the first, free copy ($2.00 per 100,000).
	PriceTrailCopyEventUSD = 2.0 / 100000
	// EventBytes is the size assumed for one CloudTrail event, for the
	// volume a CloudTrail Lake store or a log pipeline takes in.
	EventBytes = 1500
)

// meteredAPIs are the AWS APIs, by SDK service ID, that bill every request
// on their own bill, and the price of one request.
var meteredAPIs = map[string]float64{
	"S3":  PriceS3RequestUSD,
	"KMS": PriceKMSRequestUSD,
	"SQS": PriceSQSRequestUSD,
}

// Metered reports whether a request to an API, named by its SDK service ID
// ("S3", "KMS"), is billed per request.
func Metered(serviceID string) bool {
	_, ok := meteredAPIs[serviceID]
	return ok
}

// meteredSources maps a collector unit service onto the API its metered
// requests go to.
var meteredSources = map[string]string{
	"s3":         "S3",
	unitS3Policy: "S3",
	unitKMS:      "KMS",
	"sqs":        "SQS",
}

// MeteredPriceUSD is the price of one metered request a collector unit
// service makes; zero when the source makes none.
func MeteredPriceUSD(unitService string) float64 {
	return meteredAPIs[meteredSources[unitService]]
}

// Optional is a source Versus reads only when the account already pays for
// it. Versus never turns one on; the page shows AWS's price before the user
// does.
type Optional struct {
	ID string `json:"id"`
	// Name is what the user knows the feature as.
	Name string `json:"name"`
	// Price is AWS's price, in AWS's words.
	Price string `json:"price"`
	// What is what turning it on would add to Versus.
	What string `json:"what"`
	// Unit is the collector unit service whose stored gaps say the feature
	// is off in an account; empty when no collector reads it yet.
	Unit string `json:"-"`
}

// optionals are the billed AWS features Versus can read once they are on.
var optionals = []Optional{
	{ID: "access-analyzer-unused", Name: "Access Analyzer unused access", Price: "About $0.20 per identity a month",
		What: "Finds unused roles and keys"},
	{ID: "guardduty", Name: "GuardDuty", Price: "From $4.00 per million CloudTrail management events analysed, plus other data sources",
		What: "Finds threats and attack sequences"},
	{ID: "inspector", Name: "Inspector", Price: "Priced per instance, container image and function scanned a month",
		What: "Finds exploitable software on reachable hosts"},
	{ID: "security-hub", Name: "Security Hub", Price: "Priced per security check and per finding ingested a month",
		What: "Collects findings from every security service"},
	{ID: "config", Name: "AWS Config", Price: "About $0.003 per configuration item recorded",
		What: "Configuration history between reads"},
	{ID: "cloudtrail-lake", Name: "CloudTrail Lake", Price: "$0.75 per GB ingested (1-year retention) or $2.50 per GB (7-year)",
		What: "Queries over months of events"},
	{ID: "aws-health", Name: "AWS Health API", Price: "Needs a Business, Enterprise On-Ramp or Enterprise support plan",
		What: "AWS events and scheduled maintenance that affect your resources", Unit: "aws-health"},
}

// Optionals returns the billed features Versus can read once they are on,
// in display order. The slice is a copy.
func Optionals() []Optional { return append([]Optional(nil), optionals...) }
