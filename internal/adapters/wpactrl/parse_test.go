package wpactrl

import (
	"strings"
	"testing"

	"netcfg/internal/domain"
)

const sample = "bssid / frequency / signal level / flags / ssid\n" +
	"00:11:22:33:44:55\t2412\t-40\t[WPA2-PSK-CCMP][ESS]\tMy Home\n" +
	"00:11:22:33:44:66\t5180\t-70\t[WPA2-PSK-CCMP][WPA3-SAE][ESS]\tMy Home 5G\n" +
	"aa:bb:cc:dd:ee:ff\t2437\t-55\t[ESS]\tCoffee Shop\n" +
	"11:22:33:44:55:66\t2462\t-80\t[WEP][ESS]\tLegacy Net\n" +
	"22:33:44:55:66:77\t2412\t-30\t[WPA2-PSK-CCMP][ESS]\tMy Home\n" +
	"aa:bb:cc:00:00:01\t2412\t-60\t[ESS]\t\n"

func TestParseScanResults(t *testing.T) {
	got := parseScanResults(sample)

	if len(got) != 4 {
		t.Fatalf("expected 4 unique SSIDs, got %d: %+v", len(got), got)
	}
	if got[0].SSID != "My Home" || got[0].Signal != -30 {
		t.Fatalf("must keep the strongest AP per SSID, got %+v", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Signal < got[i].Signal {
			t.Fatal("results must be sorted by descending signal")
		}
	}

	byName := map[string]domain.AccessPoint{}
	for _, ap := range got {
		byName[ap.SSID] = ap
	}
	if byName["My Home 5G"].Security != domain.SecPSKSAE {
		t.Fatalf("a transition mode network must be psk-sae, got %s", byName["My Home 5G"].Security)
	}
	if byName["My Home 5G"].Band != "5 GHz" {
		t.Fatalf("wrong band: %s", byName["My Home 5G"].Band)
	}
	if byName["Coffee Shop"].Security != domain.SecOpen {
		t.Fatal("an unencrypted network must be reported as open")
	}
	if byName["Legacy Net"].Security != domain.SecWEP {
		t.Fatal("WEP must be recognised so the layer above can refuse it")
	}
}

func TestParseScanResultsIgnoresGarbage(t *testing.T) {
	inputs := []string{
		"",
		"\n\n\n",
		"not a table at all",
		"00:11:22:33:44:55\t2412",
		"00:11:22:33:44:55\tabc\txyz\t[ESS]\tName",
	}
	for _, in := range inputs {
		if got := parseScanResults(in); len(got) > 1 {
			t.Fatalf("garbage input produced %d results: %q", len(got), in)
		}
	}
}

// FuzzParseScanResults guards the only parser fed by the untrusted RF environment.
func FuzzParseScanResults(f *testing.F) {
	f.Add(sample)
	f.Add("00:11:22:33:44:55\t2412\t-40\t[ESS]\tA\n")
	f.Add("\t\t\t\t\n")

	f.Fuzz(func(t *testing.T, input string) {
		for _, ap := range parseScanResults(input) {
			if err := domain.ValidateSSID(ap.SSID); err != nil {
				t.Fatalf("parser returned an invalid SSID %q: %v", ap.SSID, err)
			}
			if ap.Quality < 0 || ap.Quality > 100 {
				t.Fatalf("quality out of range: %d", ap.Quality)
			}
			if strings.ContainsAny(ap.SSID, "\x00\r\n") {
				t.Fatal("SSID contains control characters")
			}
		}
	})
}
