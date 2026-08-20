package i18n

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"netcfg/internal/domain"
)

func TestDefaultIsVietnamese(t *testing.T) {
	if Default != Vietnamese {
		t.Fatalf("the default language must be Vietnamese, got %s", Default)
	}
	if got := Supported()[0]; got != Default {
		t.Fatalf("the default language must be listed first, got %s", got)
	}
}

func TestResolvePrecedence(t *testing.T) {
	tests := []struct {
		name                   string
		query, cookie, browser string
		want                   Lang
	}{
		{name: "no preference falls back to the default", want: Default},
		{name: "explicit choice wins", query: "en", cookie: "vi", browser: "vi", want: English},
		{name: "cookie beats the browser", cookie: "en", browser: "vi", want: English},
		{name: "browser is used last", browser: "en-GB,en;q=0.9", want: English},
		{name: "unknown values are ignored", query: "de", cookie: "fr", browser: "ja", want: Default},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Resolve(tc.query, tc.cookie, tc.browser); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestTranslateAppliesArguments(t *testing.T) {
	got := T(Vietnamese, "gateway %s is outside subnet %s", "10.0.0.1", "192.168.1.0/24")
	if !strings.Contains(got, "10.0.0.1") || !strings.Contains(got, "192.168.1.0/24") {
		t.Fatalf("arguments were lost: %s", got)
	}
	if got == "gateway 10.0.0.1 is outside subnet 192.168.1.0/24" {
		t.Fatal("the Vietnamese catalog did not translate a known string")
	}
}

func TestUnknownStringFallsBackToEnglish(t *testing.T) {
	const source = "this string is deliberately absent from every catalog"
	if got := T(Vietnamese, source); got != source {
		t.Fatalf("got %q, want the untranslated source", got)
	}
}

func TestMessageKeepsArgumentsAcrossTranslation(t *testing.T) {
	msg := domain.Msg("interface %q does not exist", "wlan9")

	if got := M(English, msg); got != `interface "wlan9" does not exist` {
		t.Fatalf("English rendering is wrong: %s", got)
	}
	if got := M(Vietnamese, msg); !strings.Contains(got, `"wlan9"`) {
		t.Fatalf("the Vietnamese rendering lost its argument: %s", got)
	}
}

// TestCatalogVerbsMatch is the guard against a translation that would panic or
// print garbage because it changed the number or order of format verbs.
func TestCatalogVerbsMatch(t *testing.T) {
	verbs := regexp.MustCompile(`%[sqdv]`)

	for lang, catalog := range catalogs {
		for source, translated := range catalog {
			if strings.HasPrefix(source, "_") || translated == "" {
				continue
			}
			want := verbs.FindAllString(source, -1)
			got := verbs.FindAllString(translated, -1)
			if len(want) != len(got) {
				t.Errorf("%s: %q has %d verbs, translation has %d", lang, source, len(want), len(got))
				continue
			}
			for i := range want {
				if want[i] != got[i] {
					t.Errorf("%s: %q verb %d is %s, translation uses %s", lang, source, i, want[i], got[i])
				}
			}
		}
	}
}

// TestEverySourceStringIsTranslated keeps the Vietnamese catalog honest as the
// code grows: any new user facing string must be added to it.
func TestEverySourceStringIsTranslated(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	pattern := regexp.MustCompile(`(?:domain\.)?(?:Invalid|NotFound|Conflict|Unavailable|Internal|Msg)\("((?:[^"\\]|\\.)+)"`)
	catalog := catalogs[Vietnamese]

	var missing []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range pattern.FindAllStringSubmatch(string(data), -1) {
			source := strings.ReplaceAll(match[1], `\"`, `"`)
			if _, ok := catalog[source]; !ok {
				missing = append(missing, source)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, source := range missing {
		t.Errorf("missing Vietnamese translation for %q", source)
	}
}

// TestEveryBrowserStringIsTranslated covers the other half of the interface.
// t() and tm() fall back to the English source when a key is absent, so a
// missing translation is invisible: the page simply stays in English.
func TestEveryBrowserStringIsTranslated(t *testing.T) {
	root := filepath.Join("..", "..", "httpapi", "assets", "static")
	pattern := regexp.MustCompile(`\bt\(\s*"((?:[^"\\]|\\.)*)"`)
	catalog := catalogs[Vietnamese]

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range pattern.FindAllStringSubmatch(string(data), -1) {
			source := strings.ReplaceAll(match[1], `\"`, `"`)
			if source == "" || seen[source] {
				continue
			}
			seen[source] = true
			if _, ok := catalog[source]; !ok {
				t.Errorf("%s: missing Vietnamese translation for %q", entry.Name(), source)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("scanned no browser strings; the pattern or the path is wrong")
	}
}

// TestEveryTemplateStringIsTranslated covers the third place user facing text
// lives. Templates render {{call .T "..."}}, which also falls back silently.
func TestEveryTemplateStringIsTranslated(t *testing.T) {
	root := filepath.Join("..", "..", "httpapi", "assets", "templates")
	pattern := regexp.MustCompile(`\.T\s+"((?:[^"\\]|\\.)*)"`)
	catalog := catalogs[Vietnamese]

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".html") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range pattern.FindAllStringSubmatch(string(data), -1) {
			source := strings.ReplaceAll(match[1], `\"`, `"`)
			if source == "" || seen[source] {
				continue
			}
			seen[source] = true
			if _, ok := catalog[source]; !ok {
				t.Errorf("%s: missing Vietnamese translation for %q", entry.Name(), source)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("scanned no template strings; the pattern or the path is wrong")
	}
}
