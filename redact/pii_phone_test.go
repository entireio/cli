package redact

import "testing"

func TestDetectPII_InternationalPhones(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIPhone: true},
	}
	redacted := []struct{ name, input, want string }{
		{"IT mobile", "chiamami al +39 347 000 0001 domani", "chiamami al [REDACTED_PHONE] domani"},
		{"IT country code in parentheses", "Tel. (+39) 347 000 0001", "Tel. [REDACTED_PHONE]"},
		{"country code in parentheses after a dot", "Tel.(+44) 20 7946 0958", "Tel.[REDACTED_PHONE]"},
		{"country code in parentheses after a word", "tel(+39) 347 000 0001", "tel[REDACTED_PHONE]"},
		{"after a dot", "Tel.+44 20 7946 0958", "Tel.[REDACTED_PHONE]"},
		{"after a dot, with dashes", "Fax.+86-755-12345678", "Fax.[REDACTED_PHONE]"},
		{"tel URI", "tel://+442079460958", "tel://[REDACTED_PHONE]"},
		{"sms URI", "sms://+442079460958", "sms://[REDACTED_PHONE]"},
		{"trunk zero, then dots", "+33 (0)1.99.00.12.34", "[REDACTED_PHONE]"},
		{"UK trunk zero, then dots", "+44 (0)20.7946.0958", "[REDACTED_PHONE]"},
		{"trunk zero and a space, then dots", "+33 (0) 1.99.00.12.34", "[REDACTED_PHONE]"},
		{"UK trunk zero and a space, then dots", "+44 (0) 20.7946.0958", "[REDACTED_PHONE]"},
		{"country code in parentheses, then dots", "(+33) 1.99.00.12.34", "[REDACTED_PHONE]"},
		{"DE slash after the area code", "Tel. +49 30/23125678", "Tel. [REDACTED_PHONE]"},
		{"DE slash, trunk zero", "+49 (0)30/23125678", "[REDACTED_PHONE]"},
		{"tabs", "+44\t20\t7946\t0958", "[REDACTED_PHONE]"},
		{"no-break spaces", "Tel.\u00a0+44\u00a020\u00a07946\u00a00958", "Tel.\u00a0[REDACTED_PHONE]"},
		{"no-break spaces and a number", "+44\u00a020\u00a07946\u00a00958\u00a02026", "[REDACTED_PHONE]\u00a02026"},
		{"no-break space after a long first group", "+447700\u00a0900123", "[REDACTED_PHONE]"},
		{"repeated spaces", "+44  20 7946 0958", "[REDACTED_PHONE]"},
		{"spaces and tabs mixed", "+44 \t20\t 7946  0958", "[REDACTED_PHONE]"},
		{"repeated spaces before a dotted number", "+33   1.99.00.12.34", "[REDACTED_PHONE]"},
		{"aligned column after a number", "+44 20 7946 0958     2026", "[REDACTED_PHONE]     2026"},
		{"repeated spaces before a word", "call +44 20 7946 0958   9am", "call [REDACTED_PHONE]   9am"},
		{"AR mobile", "WhatsApp: +54 9 11 0000-0001", "WhatsApp: [REDACTED_PHONE]"},
		{"UK London", "call +44 20 7946 0958.", "call [REDACTED_PHONE]."},
		{"UK trunk zero", "+44 (0)20 7946 0958", "[REDACTED_PHONE]"},
		{"UK mobile", "mobile +44 7700 900123", "mobile [REDACTED_PHONE]"},
		{"UK without separators", "tel:+442079460958", "tel:[REDACTED_PHONE]"},
		{"DE Berlin", "+49 30 23125678", "[REDACTED_PHONE]"},
		{"DE dashes", "phone=+49-30-23125678&x=1", "phone=[REDACTED_PHONE]&x=1"},
		{"DE trunk zero", "+49 (0)30 23125678", "[REDACTED_PHONE]"},
		{"FR spaces", "+33 1 99 00 12 34", "[REDACTED_PHONE]"},
		{"FR dots", "+33.1.99.00.12.34", "[REDACTED_PHONE]"},
		{"FR space after the country code, then dots", "+33 1.99.00.12.34", "[REDACTED_PHONE]"},
		{"ES", "+34 600 000 001", "[REDACTED_PHONE]"},
		{"ES dots, not an address", "+34.912.345.678", "[REDACTED_PHONE]"},
		{"RU", "+7 495 000-00-01", "[REDACTED_PHONE]"},
		{"RU area code in parentheses", "+7 (495) 000-00-01", "[REDACTED_PHONE]"},
		{"BR", "+55 (11) 91234-5678", "[REDACTED_PHONE]"},
		{"JP", "+81 3-1234-5678", "[REDACTED_PHONE]"},
		{"IN", "+91 98765 43210", "[REDACTED_PHONE]"},
		{"longer first group before a space", "+447700 900123", "[REDACTED_PHONE]"},
		{"in parentheses", "(+44 20 7946 0958)", "([REDACTED_PHONE])"},
		{"without separators in parentheses", "(+442079460958)", "([REDACTED_PHONE])"},
		{"without separators, then a slash", "+442079460958/123", "[REDACTED_PHONE]/123"},
		{"closing parenthesis of the text around it", "(tel +442079460958) ok", "(tel [REDACTED_PHONE]) ok"},
		{"attached extension", "+44 20 7946 0958x123", "[REDACTED_PHONE]x123"},
		{"attached ext extension", "+33 1 99 00 12 34ext123", "[REDACTED_PHONE]ext123"},
		{"attached Ext. extension", "+44 20 7946 0958Ext.12", "[REDACTED_PHONE]Ext.12"},
		{"attached ext and a space", "+44 20 7946 0958ext 123", "[REDACTED_PHONE]ext 123"},
		{"attached ext and a space after a short group", "+33 1 99 00 12 34ext 123", "[REDACTED_PHONE]ext 123"},
		{"attached x and a space without separators", "+442079460958x 123", "[REDACTED_PHONE]x 123"},
		{"number after a separator", "+44 20 7946 0958 2026 edition", "[REDACTED_PHONE] 2026 edition"},
		{"word starting with digits after a space", "call +44 20 7946 0958 9am tomorrow", "call [REDACTED_PHONE] 9am tomorrow"},
		{"known limit: a number after a dotted number goes with it", "+33 1.99.00.12.34 2026", "[REDACTED_PHONE]"},
		{"known limit: a number after a number with dots only goes with it", "+33.1.99.00.12.34 5 rue", "[REDACTED_PHONE] rue"},
		{"known limit: a date after a number goes with it", "+44 20 7946 0958  1/2/3", "[REDACTED_PHONE]"},
		{"known limit: a date after a number with a slash goes with it", "+49 30/23125678 1/2", "[REDACTED_PHONE]"},
		{"number and a decimal", "+44 20 7946 0958 1.5", "[REDACTED_PHONE]"},
		{"without separators, a tab and a decimal", "+442079460958\t1.5", "[REDACTED_PHONE]"},
		{"last groups joined by a dot", "+49 30123456 78.90", "[REDACTED_PHONE]"},
		{"in JSON", `{"phone":"+44 20 7946 0958"}`, `{"phone":"[REDACTED_PHONE]"}`},
		{"after an escape left in the text", `first\n+44 20 7946 0958`, `first\n[REDACTED_PHONE]`},
		{"two numbers", "+44 20 7946 0958, +39 347 000 0001", "[REDACTED_PHONE], [REDACTED_PHONE]"},
		{"NANP unchanged", "call +1 555 123 4567 now", "call [REDACTED_PHONE] now"},
		// Text that is not a phone number but has the shape of one, redacted
		// as docs/security-and-privacy.md says. A change that keeps one of
		// these is a fix, not a regression.
		{"known limit: digits written one by one", "+2 0 0 0 1 0 0 0 1", "[REDACTED_PHONE]"},
		{"known limit: date with dashes and a time", "+29-10-2024 12:30:00", "[REDACTED_PHONE]:30:00"},
		{"known limit: signed integer", "max = +2147483647", "max = [REDACTED_PHONE]"},
		{"known limit: numbers in aligned columns", "// +4        3  0123456789", "// [REDACTED_PHONE]"},
	}
	for _, tt := range redacted {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := redactPIIOnly(cfg, tt.input); got != tt.want {
				t.Errorf("redactPIIOnly(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}

	kept := []struct{ name, input string }{
		{"semver build metadata", "v1.2.3+20241029120000"},
		{"version after a plus", "bump to +2.10.3"},
		{"IPv4 after a plus", "+233.252.18.110"},
		{"IPv4 with nine digits", "+203.0.113.45"},
		{"signed decimal", "lat +48.8566140"},
		{"signed decimal and a number", "lat +48.8566140 2026"},
		{"signed decimal and an aligned number", "lat +48.8566140    2026"},
		{"IPv4 and a number", "+233.252.18.110 5555"},
		{"long signed decimal", "$+281475245147134.9375"},
		{"signed exponent", "8274307542972842p+726 2.92084e+234"},
		{"ISO timestamp offset", "2026-11-06T19:24:16+02:00"},
		{"git date offset", "Date:   Thu Nov 6 19:24:16 2026 +0100"},
		{"diff line of a log", "+2026-11-06 19:24:16 INFO started"},
		{"diff line of a dotted date", "+31.12.2024 23:59 Silvester"},
		{"diff line of a compact date", "+20261106"},
		{"ULID", "+01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		{"long ID", "+2345678901234567890"},
		{"base64", "ab/+2345678901/cd"},
		{"base64 with two slashes", "ab//+2345678901/cd"},
		{"date with slashes", "+31/12/2024 23:59 Silvester"},
		{"compact date with slashes", "+2026/11/06"},
		{"identifier on the left", "x+23456789012"},
		{"identifier on the right", "+442079460958abc"},
		{"known limit: a hex-like suffix keeps the number visible", "+442079460958x1f"},
		{"scientific notation", "6.02e+23"},
		{"signed count", "+250000 rows"},
		{"too few digits", "+44 20 79"},
		{"national format", "020 7946 0958"},
	}
	for _, tt := range kept {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := redactPIIOnly(cfg, tt.input); got != tt.input {
				t.Errorf("redactPIIOnly(%q) = %q, want unchanged", tt.input, got)
			}
		})
	}
}

// TestDetectPII_InternationalPhoneCountsOnce checks that the North American
// rule's match inside an international number (347 000 0001 in
// +39 347 000 0001) does not count as a second phone number.
func TestDetectPII_InternationalPhoneCountsOnce(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIPhone: true},
	}
	regions := detectPII(cfg, "+39 347 000 0001")
	if len(regions) != 1 {
		t.Fatalf("detectPII() = %v, want one region", regions)
	}
	if r := regions[0]; r.start != 0 || r.end != len("+39 347 000 0001") || r.label != labelPhone {
		t.Errorf("detectPII() = %+v, want the whole number as PHONE", r)
	}
}

// TestDetectPII_InternationalPhoneCountsOnceAcrossLabels checks that a region
// of another label sorting between the two phone matches does not keep the
// nested one.
func TestDetectPII_InternationalPhoneCountsOnceAcrossLabels(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:        true,
		Categories:     map[PIICategory]bool{PIIPhone: true},
		CustomPatterns: map[string]string{"country": `39`},
	}
	var phones int
	for _, r := range detectPII(cfg, "+39 347 000 0001") {
		if r.label == labelPhone {
			phones++
		}
	}
	if phones != 1 {
		t.Errorf("detectPII() found %d PHONE regions, want 1", phones)
	}
}
