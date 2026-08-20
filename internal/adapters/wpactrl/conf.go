package wpactrl

import (
	"bufio"
	"os"
	"regexp"
	"strings"

	"netcfg/internal/domain"
)

var hashedPSKRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// confBlock is one network={...} entry of a wpa_supplicant configuration file.
type confBlock struct {
	fields map[string]string
}

// parseConfBlocks splits a wpa_supplicant configuration into its network blocks.
// The format is line oriented, so a brace depth counter is enough; values keep
// their quotes because that is what distinguishes a passphrase from a raw key.
func parseConfBlocks(r *bufio.Scanner) []confBlock {
	var out []confBlock
	var current *confBlock

	for r.Scan() {
		line := strings.TrimSpace(r.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case current == nil && strings.HasPrefix(line, "network={"):
			current = &confBlock{fields: map[string]string{}}
		case current != nil && line == "}":
			out = append(out, *current)
			current = nil
		case current != nil:
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			current.fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return out
}

// unquote returns the contents of a quoted wpa_supplicant value.
func unquote(v string) (string, bool) {
	if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
		return v[1 : len(v)-1], true
	}
	return v, false
}

// secretOf extracts the credential of the block matching ssid.
func secretOf(blocks []confBlock, ssid string) (domain.ProfileSecret, bool) {
	for _, b := range blocks {
		name, quoted := unquote(b.fields["ssid"])
		if !quoted || name != ssid {
			continue
		}
		if sae, ok := unquote(b.fields["sae_password"]); ok && sae != "" {
			return domain.ProfileSecret{SSID: ssid, Value: domain.NewSecret(sae)}, true
		}
		psk := b.fields["psk"]
		if plain, ok := unquote(psk); ok {
			return domain.ProfileSecret{SSID: ssid, Value: domain.NewSecret(plain)}, true
		}
		if hashedPSKRe.MatchString(psk) {
			return domain.ProfileSecret{SSID: ssid, Value: domain.NewSecret(psk), Hashed: true}, true
		}
		return domain.ProfileSecret{SSID: ssid}, true
	}
	return domain.ProfileSecret{}, false
}

// readConfSecret looks the credential up in the first configuration file that
// contains the network. Per-interface files take precedence over the shared one.
func readConfSecret(dir, link, ssid string) (domain.ProfileSecret, error) {
	names := []string{"wpa_supplicant-" + link + ".conf", "wpa_supplicant.conf"}
	for _, name := range names {
		f, err := os.Open(dir + "/" + name)
		if err != nil {
			continue
		}
		secret, found := secretOf(parseConfBlocks(bufio.NewScanner(f)), ssid)
		_ = f.Close()
		if found {
			return secret, nil
		}
	}
	return domain.ProfileSecret{}, domain.NotFound("no stored credential found for %q in %s", ssid, dir)
}
