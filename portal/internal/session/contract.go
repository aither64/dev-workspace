package session

import "strconv"

// These values project the shared request boundary and worst-case transport
// encoding overhead published in runtime-contract.json.
const (
	MaxMessageBytes         = 20_000
	FormEncodingExpansion   = 3
	JSONEncodingExpansion   = 6
	TransportEnvelopeBytes  = 1_024
	MaxFormRequestBodyBytes = MaxMessageBytes*FormEncodingExpansion + TransportEnvelopeBytes
	MaxJSONRequestBodyBytes = MaxMessageBytes*JSONEncodingExpansion + TransportEnvelopeBytes
)

// FormattedMaxMessageBytes returns the published limit for user-visible text.
func FormattedMaxMessageBytes() string {
	digits := strconv.Itoa(MaxMessageBytes)
	for index := len(digits) - 3; index > 0; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	return digits
}
