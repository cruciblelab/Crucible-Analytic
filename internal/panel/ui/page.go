package ui

import "time"

// NoticeLevel is the closed set of banner kinds. Closed because the
// class it maps to is written into the page's HTML and the role it maps
// to is what a screen reader announces: neither is a place for a string
// that arrived from anywhere else.
type NoticeLevel string

const (
	// NoticeInfo states something true the reader may not know.
	NoticeInfo NoticeLevel = "bilgi"
	// NoticeWarn precedes an action that can go wrong. The developer
	// mode warning is this, and only this - it is not a barrier.
	NoticeWarn NoticeLevel = "uyari"
	// NoticeError reports something that already went wrong.
	NoticeError NoticeLevel = "hata"
	// NoticeLock explains why a control is absent. Its own level rather
	// than a warning, because nothing has gone wrong and nothing is
	// about to: the reader is being told where the boundary is.
	NoticeLock NoticeLevel = "kilit"
)

// Class is the CSS class for this level.
func (l NoticeLevel) Class() string {
	switch l {
	case NoticeInfo, NoticeWarn, NoticeError, NoticeLock:
		return string(l)
	default:
		return string(NoticeInfo)
	}
}

// Role is the ARIA role. Only genuine errors get "alert", which
// interrupts a screen reader mid-sentence; using it for the lock notice
// would train people to ignore it.
func (l NoticeLevel) Role() string {
	if l == NoticeError {
		return "alert"
	}
	return "note"
}

// Notice is a banner above the page content.
//
// The text is a value, not a catalog key, because most notices in this
// panel are written by the rule they explain - a setting's lock reason
// lives beside the setting, not in the interface catalog. See the
// comment at the top of messages.tr.toml.
type Notice struct {
	Level       NoticeLevel
	Title       string
	Body        string
	ActionURL   string
	ActionLabel string
}

// NavItem is one link in the header.
type NavItem struct {
	Label   string
	URL     string
	Current bool
}

// UserView is what the chrome says about who is looking.
//
// It is a rendering view, not an authority: the fields describe what to
// draw, and every decision behind them was already made by the panel
// package. Nothing in ui may consult it to decide whether something is
// allowed.
type UserView struct {
	// Label is the name in the corner - an email, or the fixed
	// developer marker.
	Label string
	// Operator means this person reaches the site by running the
	// deployment rather than by a membership row, and the chrome says
	// so. Hiding it would let an operator forget whose data they are
	// looking at.
	Operator bool
	// ReadOnly means the account cannot change anything here. Said once
	// in the chrome so that every missing button on the page below has
	// an explanation the reader has already seen.
	ReadOnly bool
	// DeveloperMode reports whether the technical layers are currently
	// showing.
	DeveloperMode bool
}

// SiteView identifies which site's numbers are on the page. A panel
// with several sites and no visible name is a panel that will
// eventually show somebody the wrong one.
type SiteView struct {
	ID   string
	Name string
}

// Page is what every template receives.
//
// One struct rather than a per-page type because the chrome - header,
// notices, footer, formatter - is identical everywhere, and a partial
// that receives the whole page can always reach it. Page-specific
// values go in Data.
type Page struct {
	// Title goes in <title>, already in Turkish.
	Title string
	// Heading is the <h1>. Empty renders no heading, for pages that
	// draw their own.
	Heading string
	Nav     []NavItem
	User    UserView
	Site    SiteView
	Notices []Notice
	// CSRF is the token for forms on this page. Handlers set it; the
	// renderer never invents one.
	CSRF string
	// Version is the build the panel is running.
	Version string
	// Data is whatever this page needs.
	Data any

	// F formats numbers and times in the site's zone. Set by the
	// renderer when a handler leaves it nil, so a template can always
	// call it.
	F *Formatter
}

// errorPage is Data for the error template.
type errorPage struct {
	Status int
	Body   string
	// At and Reference appear only on server-side failures, and exist
	// so a support conversation can start with something that can be
	// found in the log rather than with "it said something went wrong".
	At        time.Time
	Reference string
	BackURL   string
	BackLabel string
}
