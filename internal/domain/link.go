package domain

import "regexp"

var linkNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,14}$`)

// Link is a network interface as presented to the operator.
type Link struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac"`
	Wireless  bool     `json:"wireless"`
	AdminUp   bool     `json:"adminUp"`
	OperUp    bool     `json:"operUp"`
	Addresses []string `json:"addresses"`
	Gateway   string   `json:"gateway"`
	DNS       []string `json:"dns"`
}

// ValidateLinkName rejects names that could escape a file path or a command.
// Callers must still check the name against the interfaces that really exist.
func ValidateLinkName(name string) error {
	if !linkNameRe.MatchString(name) || name == "." || name == ".." {
		return Invalid("invalid interface name: %q", name)
	}
	return nil
}

// FindLink returns the link with the given name.
func FindLink(links []Link, name string) (Link, error) {
	if err := ValidateLinkName(name); err != nil {
		return Link{}, err
	}
	for _, l := range links {
		if l.Name == name {
			return l, nil
		}
	}
	return Link{}, NotFound("interface %q does not exist", name)
}
