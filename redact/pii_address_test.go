package redact

import (
	"strings"
	"testing"
)

func addressOnlyConfig() *PIIConfig {
	return &PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIAddress: true},
	}
}

func TestDetectPII_EuropeanAddresses(t *testing.T) {
	t.Parallel()
	cfg := addressOnlyConfig()
	tests := []struct{ input, want string }{
		// The street line is replaced; postal code and city stay.
		{"Via Roma 12, 20121 Milano", "[REDACTED_ADDRESS], 20121 Milano"},
		{"Calle Mayor 5, 28013 Madrid", "[REDACTED_ADDRESS], 28013 Madrid"},
		{"12 rue de la Paix, 75002 Paris", "[REDACTED_ADDRESS], 75002 Paris"},
		{"Hauptstraße 12, 10115 Berlin", "[REDACTED_ADDRESS], 10115 Berlin"},

		// Italian
		{"ship to Piazza del Duomo 1 please", "ship to [REDACTED_ADDRESS] please"},
		{"Via Giuseppe Garibaldi, 5", "[REDACTED_ADDRESS]"},
		{"Corso Vittorio Emanuele II 15", "[REDACTED_ADDRESS]"},
		{"Via dei Mille 3/A", "[REDACTED_ADDRESS]"},
		{"Viale dell'Indipendenza 8", "[REDACTED_ADDRESS]"},
		{"Largo Sant'Ambrogio 2", "[REDACTED_ADDRESS]"},
		{"Via Roma n. 12", "[REDACTED_ADDRESS]"},
		{"Via XX Settembre 20", "[REDACTED_ADDRESS]"},
		{"Via 20 Settembre 97", "[REDACTED_ADDRESS]"},

		// Spanish
		{"Calle de Alcalá, 42", "[REDACTED_ADDRESS]"},
		{"Avenida de la Constitución 3", "[REDACTED_ADDRESS]"},
		{"Paseo de la Castellana 259", "[REDACTED_ADDRESS]"},
		{"C/ Mayor 5", "[REDACTED_ADDRESS]"},
		{"C/Mayor 5", "[REDACTED_ADDRESS]"},
		{"Plaza Mayor nº 1", "[REDACTED_ADDRESS]"},
		{"Avda. Diagonal 640", "[REDACTED_ADDRESS]"},
		{"Calle Mayor n.º 1", "[REDACTED_ADDRESS]"},
		{"Avenida 9 de Julio 1000", "[REDACTED_ADDRESS]"},

		// French
		{"5 avenue des Champs-Élysées", "[REDACTED_ADDRESS]"},
		{"8 bis boulevard Saint-Germain", "[REDACTED_ADDRESS]"},
		{"3 place Vendôme", "[REDACTED_ADDRESS]"},
		{"12, rue de l'Université", "[REDACTED_ADDRESS]"},
		{"1 rue d'Alésia", "[REDACTED_ADDRESS]"},
		{"12 Rue du Faubourg Saint-Honoré", "[REDACTED_ADDRESS]"},
		{"12 B rue de la Paix", "[REDACTED_ADDRESS]"},
		{"12 rue du 8 Mai 1945, 75000 Paris", "[REDACTED_ADDRESS], 75000 Paris"},
		{"12 rue du 8 Mai 75000 Paris", "[REDACTED_ADDRESS] 75000 Paris"},

		// German
		{"Karl-Marx-Straße 5", "[REDACTED_ADDRESS]"},
		{"Berliner Str. 5", "[REDACTED_ADDRESS]"},
		{"Goethestr. 10", "[REDACTED_ADDRESS]"},
		{"Lindenallee 4a", "[REDACTED_ADDRESS]"},
		{"Bahnhofstrasse 1", "[REDACTED_ADDRESS]"},
		{"Frankfurter Allee 11", "[REDACTED_ADDRESS]"},
		{"Johann-Sebastian-Bachstraße 12", "[REDACTED_ADDRESS]"},
		{"Konrad-Adenauer-Platz 1", "[REDACTED_ADDRESS]"},
		{"Hauptstraße\t12", "[REDACTED_ADDRESS]"},
		{"Alte Schönhauser Straße 7", "[REDACTED_ADDRESS]"},
		{"Große Hamburger Straße 7", "[REDACTED_ADDRESS]"},
		{"Alte Jakobstraße 5", "[REDACTED_ADDRESS]"},

		// Decomposed accents (e followed by U+0301) stay inside the name.
		{"12 rue de la Re\u0301publique", "[REDACTED_ADDRESS]"},
		{"3 place de l'E\u0301glise", "[REDACTED_ADDRESS]"},
		{"2 alle\u0301e des Roses", "[REDACTED_ADDRESS]"},
		{"12 rue du 8 fe\u0301vrier 1962", "[REDACTED_ADDRESS]"},
		{"3 place du 15 Aou\u0302t", "[REDACTED_ADDRESS]"},

		// A postal code right after a comma is not a decimal.
		{"Via Roma 12,20121 Milano", "[REDACTED_ADDRESS],20121 Milano"},
		{"Hauptstraße 12,10115 Berlin", "[REDACTED_ADDRESS],10115 Berlin"},

		// A street type joined onto a word does not start a match, and does
		// not hide the street line after it.
		{"xVia Foo Via Roma 12", "xVia Foo [REDACTED_ADDRESS]"},

		// A match stays on its line: the French name has no house number
		// after it to stop it, so it must not run on into a heading.
		{"12 rue de la Paix\nDeployment Status", "[REDACTED_ADDRESS]\nDeployment Status"},
		{"Via Roma\n12 tests passed", "Via Roma\n12 tests passed"},
		{"Hauptstraße\n12 files", "Hauptstraße\n12 files"},

		// Both shapes in one string.
		{"from Via Roma 12 to 123 Main Street", "from [REDACTED_ADDRESS] to [REDACTED_ADDRESS]"},
	}
	for _, tt := range tests {
		if got := redactPIIOnly(cfg, tt.input); got != tt.want {
			t.Errorf("redactPIIOnly(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDetectPII_EuropeanAddressLookalikes(t *testing.T) {
	t.Parallel()
	cfg := addressOnlyConfig()
	tests := []string{
		// Via in prose: the name must be a title-case word, and a
		// lowercase via is English (via Python 3), not a street type.
		"Via SSH 22",
		"Via CLI 2",
		"Via the API 2",
		"installed via Python 3",
		// A house number that continues as a decimal or version.
		"Via Node 18.2",
		"Via Node 18,2",
		"Hauptstraße 12,5 km",
		"Heldeplatz 2.0MB",
		// rue in identifiers.
		"rue_de_la_paix 12",
		"item12 rue de la Paix",
		"getRue(12)",
		// Plaza as a product name.
		"Plaza 2.0",
		"Plaza API v2",
		"the Plaza Hotel",
		// Part of a longer word or number.
		"xHauptstraße 12",
		"Hauptstraße 12345",
		"Hauptstraße 12px",
		"Via Roma 12345",
		// A suffix only counts on a title-case street word.
		"String 12",
		"Weg 5",
		"hauptstraße 12",
		// A day or roman numeral only starts a name before a month.
		"Via XX Docker 3",
		"Via 2 Factor 3",
	}
	for _, input := range tests {
		if got := redactPIIOnly(cfg, input); got != input {
			t.Errorf("redactPIIOnly(%q) = %q, want unchanged", input, got)
		}
	}
}

func TestDetectPII_EuropeanAddressNeedsCategory(t *testing.T) {
	t.Parallel()
	cfg := &PIIConfig{
		Enabled:    true,
		Categories: map[PIICategory]bool{PIIEmail: true, PIIPhone: true, PIIIP: true},
	}
	for _, input := range []string{"Via Roma 12", "12 rue de la Paix", "Hauptstraße 12"} {
		if got := redactPIIOnly(cfg, input); got != input {
			t.Errorf("redactPIIOnly(%q) with address off = %q, want unchanged", input, got)
		}
	}
}

func TestHasStreetKeyword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		s    string
		k    streetKeyword
		want bool
	}{
		{"12 rue de la Paix", streetKeyword{word: "rue"}, true},
		{"return true if ok", streetKeyword{word: "rue"}, false},
		{"Caller ID", streetKeyword{word: "Calle"}, false},
		{"Calle", streetKeyword{word: "Calle"}, false},
		{`to:\nVia Roma 12`, streetKeyword{word: "Via"}, true},
		{"C/Mayor 5", streetKeyword{word: "C/", nameFollows: true}, true},
		{"Hauptstraße 12", streetKeyword{word: "straße", inWord: true}, true},
		{"Hauptstraße 12", streetKeyword{word: "straße"}, false},
		{"str.join(parts)", streetKeyword{word: "str.", inWord: true}, false},
		// A later occurrence counts when an earlier one does not.
		{"true, then 3 rue Royale", streetKeyword{word: "rue"}, true},
	}
	for _, tt := range tests {
		if got := hasStreetKeyword(tt.s, tt.k); got != tt.want {
			t.Errorf("hasStreetKeyword(%q, %+v) = %v, want %v", tt.s, tt.k, got, tt.want)
		}
	}
}

func TestPIIIntegration_EuropeanAddressAfterJSONEscape(t *testing.T) {
	ConfigurePII(*addressOnlyConfig())
	t.Cleanup(resetPIIConfig)

	// Raw JSON text: the n of the \n escape is not part of the street name.
	got := String(`deliver to:\nVia Roma 12\nthanks`)
	want := `deliver to:\n[REDACTED_ADDRESS]\nthanks`
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	out, err := JSONLContent(`{"content":"deliver to:\nHauptstraße 12, 10115 Berlin"}`)
	if err != nil {
		t.Fatalf("JSONLContent: %v", err)
	}
	if strings.Contains(out, "Hauptstraße") || !strings.Contains(out, "10115 Berlin") {
		t.Errorf("JSONLContent() = %q, want the street line redacted and the city kept", out)
	}
}
