package cli

import "strings"

// providerIdentity owns how one identity provider's accounts are spelled. The
// control plane stores a handle per identity and resolves exactly that string
// at /identity/handles; what a user sees and types need not be the same thing.
// Each provider maps between the two in one place, so what `auth status`
// shows, what `grant … list` prints and what `grant add` accepts cannot drift.
type providerIdentity interface {
	// displayHandle turns the server's stored handle into the one users see
	// and type.
	displayHandle(stored string) string
	// storedHandle turns a handle a user typed into the one the server
	// resolves. It accepts both the display form and the stored form, so a
	// value copied from --json (which carries the wire spelling) still works.
	storedHandle(typed string) string
}

const providerGoogle = "google"

// providerIdentities is keyed by the lowercase provider slug. A provider
// missing here spells its handles the same on both sides.
var providerIdentities = map[string]providerIdentity{
	providerGitHub: usernameIdentity{},
	providerGoogle: mintedIdentity{prefix: providerGoogle + "-"},
}

func identityFor(provider string) providerIdentity {
	if id, ok := providerIdentities[strings.ToLower(provider)]; ok {
		return id
	}
	return usernameIdentity{}
}

// usernameIdentity is a provider that issues real usernames (GitHub): the
// handle is the username on both sides. A user genuinely called `github-foo`
// keeps that name, because nothing is minted here to strip.
type usernameIdentity struct{}

func (usernameIdentity) displayHandle(stored string) string { return stored }
func (usernameIdentity) storedHandle(typed string) string   { return typed }

// mintedIdentity is a provider with no username concept (Google). The server
// mints the handle as "<provider>-<subject id>", which qualified would spell
// the provider twice — `google:google-100…` — so users see and type the
// subject id alone, `google:100…`, and the prefix is restored for lookup.
//
// The subject id is case-sensitive (Google documents `sub` as up to 255
// case-sensitive ASCII characters), so it is never folded. Only the fixed
// prefix is matched without regard to case, and it is always rebuilt in its
// canonical spelling so the server sees exactly what it minted.
//
// The subject id is also opaque, so it could itself begin with the prefix.
// Stripping one layer from `google-google-1001` would show `google-1001`,
// which is the stored form of a different account (subject `1001`), and a
// value pasted back from a list would grant to it. Such a handle is shown in
// its stored form instead, so every displayed value maps back to exactly one
// stored handle.
type mintedIdentity struct{ prefix string }

func (m mintedIdentity) displayHandle(stored string) string {
	rest, ok := cutPrefixFold(stored, m.prefix)
	if !ok || rest == "" {
		return stored
	}
	if _, ambiguous := cutPrefixFold(rest, m.prefix); ambiguous {
		return stored
	}
	return rest
}

func (m mintedIdentity) storedHandle(typed string) string {
	rest, _ := cutPrefixFold(typed, m.prefix)
	return m.prefix + rest
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// displayQualifiedHandle renders a provider and its stored handle as users see
// and type it ("github:alice", "google:100…").
func displayQualifiedHandle(provider, stored string) string {
	return formatQualifiedHandle(provider, identityFor(provider).displayHandle(stored))
}

// displayGranteeName renders a server-supplied grantee name for a table or
// picker row. Listings carry the qualified stored form ("google:google-100…");
// anything that is not a qualified handle (an org or team name, a ULID) is
// returned unchanged.
func displayGranteeName(name string) string {
	provider, handle, err := parseQualifiedHandle(name)
	if err != nil {
		return name
	}
	return displayQualifiedHandle(provider, handle)
}

// parseGranteeHandle splits a typed provider-qualified handle into the provider
// and the handle the server resolves — the inverse of displayQualifiedHandle.
func parseGranteeHandle(ref string) (provider, stored string, err error) {
	provider, handle, err := parseQualifiedHandle(ref)
	if err != nil {
		return "", "", err
	}
	return provider, identityFor(provider).storedHandle(handle), nil
}
