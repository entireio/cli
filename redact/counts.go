package redact

import "maps"

// Counts reports what redaction replaced, by layer, and never the values.
// Checkpoint metadata stores it, so field and layer names are a persisted
// schema.
//
// A layer present with 0 ran and found nothing; a layer absent did not run
// (custom rules or PII not configured, a scanner engine switched off).
type Counts struct {
	// Replacements is the number of placeholders written.
	Replacements int `json:"replacements"`
	// Layers maps each layer that ran to the matches it found in the values
	// it scanned, counting each span a layer flags once however many of its
	// rules match there. The sum can differ from Replacements either way: overlapping
	// matches count in every layer that found them but are replaced once, and
	// a JSON line's splice also replaces copies of a found value in fields the
	// scan skips, such as IDs.
	Layers map[string]int `json:"layers"`
	// CustomRulesActive is how many custom rules compiled and ran. Rule names
	// are left out: they can name things a team keeps out of shared metadata.
	CustomRulesActive int `json:"custom_rules_active,omitempty"`
	// PII maps each PII label that ran to its matches. Custom PII patterns
	// share the "custom" bucket, for the same reason as rule names.
	PII map[string]int `json:"pii,omitempty"`
}

// Add returns the sum of c and o without changing either. Unknown counts
// (nil) make the sum unknown. CustomRulesActive takes the larger value: it
// describes the configuration, which both operands share.
func (c *Counts) Add(o *Counts) *Counts {
	if c == nil || o == nil {
		return nil
	}
	sum := &Counts{
		Replacements:      c.Replacements + o.Replacements,
		Layers:            addCountMaps(c.Layers, o.Layers),
		CustomRulesActive: max(c.CustomRulesActive, o.CustomRulesActive),
		PII:               addCountMaps(c.PII, o.PII),
	}
	if sum.Layers == nil {
		sum.Layers = map[string]int{}
	}
	return sum
}

func addCountMaps(a, b map[string]int) map[string]int {
	if a == nil && b == nil {
		return nil
	}
	sum := maps.Clone(a)
	if sum == nil {
		sum = make(map[string]int, len(b))
	}
	for k, n := range b {
		sum[k] += n
	}
	return sum
}

// layer identifies the detection step that produced a region. The zero value
// marks a region no layer claimed; it is reported as "unattributed" rather
// than dropped, so a layer added without a tag shows up in the counts.
type layer uint8

const (
	layerUnattributed layer = iota
	layerEntropy
	layerBetterleaks
	layerGoredact
	layerProviderToken
	layerCredentialedURI
	layerConnectionString
	layerCustom
	layerShellStdin
	layerCredentialValue
	layerPII
	layerKeyContext
	layerCredentialKey
	layerCount
)

// layerNames are the persisted Counts.Layers keys.
//
//nolint:gosec // layer names, not credentials
var layerNames = [layerCount]string{
	layerUnattributed:     "unattributed",
	layerEntropy:          "entropy",
	layerBetterleaks:      "betterleaks",
	layerGoredact:         "goredact",
	layerProviderToken:    "provider_token",
	layerCredentialedURI:  "credentialed_uri",
	layerConnectionString: "connection_string",
	layerCustom:           "custom",
	layerShellStdin:       "shell_stdin",
	layerCredentialValue:  "credential_value",
	layerPII:              "pii",
	layerKeyContext:       "key_context",
	layerCredentialKey:    "credential_key",
}

// LayerNames lists the layer names Counts can report, in detection order.
func LayerNames() []string {
	return append(append([]string{}, layerNames[layerUnattributed+1:]...), layerNames[layerUnattributed])
}

// tagFrom marks regions[from:] as found by l and returns where the next
// layer's regions start.
func tagFrom(regions []taggedRegion, from int, l layer) int {
	for i := from; i < len(regions); i++ {
		regions[i].layer = l
	}
	return len(regions)
}

// PII buckets. A custom pattern whose label is a built-in one counts under
// that built-in label, matching the placeholder it writes.
const (
	piiEmail = iota
	piiPhone
	piiAddress
	piiCustom
	piiBucketCount
)

var piiBucketNames = [piiBucketCount]string{labelEmail, labelPhone, labelAddress, "custom"}

func piiBucket(label string) int {
	switch label {
	case labelEmail:
		return piiEmail
	case labelPhone:
		return piiPhone
	case labelAddress:
		return piiAddress
	default:
		return piiCustom
	}
}

// tally accumulates what one redaction pass replaced. It is fixed-size, so a
// copy taken before a line can roll that line back for free. Methods accept
// a nil receiver, which counts nothing.
type tally struct {
	replacements int
	layers       [layerCount]int
	pii          [piiBucketCount]int
}

// addRegions counts the non-empty regions found in one string, merged into
// replaced placeholders. A layer that flags the same span more than once, as
// when two of its rules match one value, counts it once.
//
// regions must be sorted by span, as redactRegions sorts them, so identical
// spans are adjacent.
func (t *tally) addRegions(regions []taggedRegion, replaced int) {
	if t == nil {
		return
	}
	t.replacements += replaced
	for i, r := range regions {
		if layerFlaggedSpanBefore(regions, i) {
			continue
		}
		t.layers[r.layer]++
		if r.layer == layerPII {
			t.pii[piiBucket(r.label)]++
		}
	}
}

// layerFlaggedSpanBefore reports whether an earlier region of the same layer
// has the same span as regions[i].
func layerFlaggedSpanBefore(regions []taggedRegion, i int) bool {
	for j := i - 1; j >= 0 && regions[j].region == regions[i].region; j-- {
		if regions[j].layer == regions[i].layer {
			return true
		}
	}
	return false
}

// addPlaceholder counts a value replaced whole by l.
func (t *tally) addPlaceholder(l layer) {
	if t == nil {
		return
	}
	t.replacements++
	t.layers[l]++
}

func (t *tally) add(o tally) {
	if t == nil {
		return
	}
	t.replacements += o.replacements
	for i, n := range o.layers {
		t.layers[i] += n
	}
	for i, n := range o.pii {
		t.pii[i] += n
	}
}

// counts reports t over every layer that runs under the current
// configuration, so a layer that ran and found nothing reads as 0.
func (t *tally) counts() *Counts {
	c := &Counts{
		Replacements:      t.replacements,
		Layers:            make(map[string]int, layerCount),
		CustomRulesActive: ActiveCustomRules(),
	}
	piiActive := activePIIBuckets()
	scanners := getScanners()
	for l := range layerCount {
		var active bool
		switch l {
		case layerEntropy, layerProviderToken, layerCredentialedURI, layerConnectionString,
			layerShellStdin, layerCredentialValue, layerKeyContext, layerCredentialKey:
			active = true
		case layerBetterleaks:
			active = scanners.betterleaks && getDetector() != nil
		case layerGoredact:
			active = scanners.engine != nil
		case layerCustom:
			active = c.CustomRulesActive > 0
		case layerPII:
			active = piiActive != [piiBucketCount]bool{}
		case layerUnattributed, layerCount:
		}
		if active || t.layers[l] > 0 {
			c.Layers[layerNames[l]] = t.layers[l]
		}
	}
	for b := range piiBucketCount {
		if piiActive[b] || t.pii[b] > 0 {
			if c.PII == nil {
				c.PII = make(map[string]int, piiBucketCount)
			}
			c.PII[piiBucketNames[b]] = t.pii[b]
		}
	}
	return c
}

// activePIIBuckets reports which PII buckets have a pattern that runs.
func activePIIBuckets() [piiBucketCount]bool {
	var active [piiBucketCount]bool
	cfg := getPIIConfig()
	if cfg == nil || !cfg.Enabled {
		return active
	}
	for _, p := range cfg.patterns {
		active[piiBucket(p.label)] = true
	}
	return active
}
