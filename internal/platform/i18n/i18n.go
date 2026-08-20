// Package i18n translates user facing text. Catalog keys are the English source
// strings themselves, gettext style, so Go code stays readable and there is no
// separate key registry to keep in sync.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"netcfg/internal/domain"
)

// Lang is a supported interface language.
type Lang string

const (
	Vietnamese Lang = "vi"
	English    Lang = "en"

	// Default is what an operator gets without an explicit preference.
	Default = Vietnamese

	// Cookie is where the chosen language is remembered.
	Cookie = "netcfg_lang"
)

//go:embed locales/*.json
var files embed.FS

var catalogs = map[Lang]map[string]string{}

func init() {
	entries, err := files.ReadDir("locales")
	if err != nil {
		panic(err)
	}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".json")
		data, err := files.ReadFile("locales/" + entry.Name())
		if err != nil {
			panic(err)
		}
		catalog := map[string]string{}
		if err := json.Unmarshal(data, &catalog); err != nil {
			panic(fmt.Sprintf("locale %s: %v", name, err))
		}
		catalogs[Lang(name)] = catalog
	}
}

// Supported lists the available languages, Default first.
func Supported() []Lang {
	out := make([]Lang, 0, len(catalogs))
	for lang := range catalogs {
		out = append(out, lang)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i] == Default) != (out[j] == Default) {
			return out[i] == Default
		}
		return out[i] < out[j]
	})
	return out
}

// Parse validates a language tag such as "vi" or "en-GB".
func Parse(value string) (Lang, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if base, _, found := strings.Cut(value, "-"); found {
		value = base
	}
	if _, ok := catalogs[Lang(value)]; ok {
		return Lang(value), true
	}
	return Default, false
}

// Resolve picks a language from an explicit choice, a cookie, then the browser
// preference, falling back to Default.
func Resolve(query, cookie, acceptLanguage string) Lang {
	if lang, ok := Parse(query); ok {
		return lang
	}
	if lang, ok := Parse(cookie); ok {
		return lang
	}
	for _, part := range strings.Split(acceptLanguage, ",") {
		tag, _, _ := strings.Cut(part, ";")
		if lang, ok := Parse(tag); ok {
			return lang
		}
	}
	return Default
}

// T translates an English source string and applies its arguments.
func T(lang Lang, source string, args ...any) string {
	format := lookup(lang, source)
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

// M translates a domain.Message, keeping the arguments produced upstream.
func M(lang Lang, m domain.Message) string {
	if m.Format == "" {
		return ""
	}
	format := lookup(lang, m.Format)
	if len(m.Args) == 0 {
		return format
	}
	args := make([]any, len(m.Args))
	for i, a := range m.Args {
		args[i] = a
	}
	return fmt.Sprintf(format, args...)
}

// Catalog returns the translations for one language, for the browser to use.
func Catalog(lang Lang) map[string]string {
	if catalog, ok := catalogs[lang]; ok {
		return catalog
	}
	return map[string]string{}
}

// lookup falls back to the source string, which is already valid English.
func lookup(lang Lang, source string) string {
	if catalog, ok := catalogs[lang]; ok {
		if translated, ok := catalog[source]; ok && translated != "" {
			return translated
		}
	}
	return source
}
