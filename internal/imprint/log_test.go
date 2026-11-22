package imprint

import (
	"bytes"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestImprintLogOutputHasNoPersonalData(t *testing.T) {
	imp := &Imprint{
		ImprintURL: "https://acme.de/impressum",
		Fields: Fields{
			CompanyName: s("Peters Sport Shop"), RepresentedBy: []string{"Peter Schwabe"},
			Address: &Address{Street: s("Brünerstr. 25"), PostalCode: s("46240"), City: s("Bottrop")},
			Contact: &Contact{Email: s("peter@example.de"), Phone: s("02041 790 308"), Fax: s("02041 790 309")},
			VATID:   s("DE123456789"), Country: s("DE"),
		},
		Text:     "Peter Schwabe, Brünerstr. 25",
		Warnings: []Warning{{Code: "a"}, {Code: "b"}},
	}
	var buf bytes.Buffer
	l := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&buf), zapcore.InfoLevel))
	l.Info("loaded", zap.Object("imprint", imp))

	out := buf.String()
	for _, secret := range []string{"Peter", "Schwabe", "Brünerstr", "46240", "Bottrop", "peter@example.de", "02041", "DE123456789", "Peters Sport Shop"} {
		if strings.Contains(out, secret) {
			t.Errorf("log line leaks %q: %s", secret, out)
		}
	}
	for _, want := range []string{`"imprint_url":"https://acme.de/impressum"`, `"country":"DE"`, `"warnings":2`, `"text_len":`} {
		if !strings.Contains(out, want) {
			t.Errorf("log line lacks %s: %s", want, out)
		}
	}
}
