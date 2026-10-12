package userapi

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/internal/plugin/users"
	"github.com/nasnet-community/mtproto-remote-plugin/telemt"
)

// Proxy is one proxy's users, as the routes below read and change them.
type Proxy interface {
	Users() []users.User
	// Edit changes the users, and the counter baseline of deleted ones, in one write.
	Edit(fn func(us *[]users.User, base map[string]users.Usage) error) error
	Links(secret string) LinksView
	Usage() users.ServerUsage // with what memory holds folded in
	Changed()                 // hands the users to the proxy
	Forget(name string)       // drops a deleted user's numbers
	Flush() error             // writes what memory holds
}

// Handlers serve a proxy's users. Lookup finds the proxy a request names.
type Handlers struct {
	Lookup func(r *http.Request) (Proxy, error)
	Fail   func(http.ResponseWriter, error)
	Now    func() time.Time
	// Here names the proxy in messages: "this server" or "this proxy".
	Here string
}

// Routes adds the users' routes under prefix, like "/api/servers/{id}".
func (h Handlers) Routes(route func(pattern string, fn http.HandlerFunc), prefix string) {
	route("GET "+prefix+"/users", h.list)
	route("POST "+prefix+"/users", h.create)
	route("POST "+prefix+"/users/bulk", h.bulk)
	route("PATCH "+prefix+"/users/{name}", h.update)
	route("DELETE "+prefix+"/users/{name}", h.remove)
	route("POST "+prefix+"/users/{name}/rotate-secret", h.rotateSecret)
	route("POST "+prefix+"/users/{name}/reset-usage", h.resetUsage)
}

// Known is the status for the errors this package and users make, if err is one.
func Known(err error) (code int, msg string, ok bool) {
	var e *Error
	var inv users.Invalid
	switch {
	case errors.As(err, &e):
		return e.Code, e.Msg, true
	case errors.As(err, &inv):
		return http.StatusBadRequest, inv.Error(), true
	}
	return 0, "", false
}

var (
	ErrNoUser    = &Error{http.StatusNotFound, "no such user"}
	errUnchanged = errors.New("nothing to change")
)

type LinksView struct {
	TG    string          `json:"tg"`
	HTTPS string          `json:"https"`
	Extra []ExtraLinkView `json:"extra"`
}

type ExtraLinkView struct {
	Domain string `json:"domain"`
	TG     string `json:"tg"`
	HTTPS  string `json:"https"`
}

// ProxyLink is built here so the page never assembles a secret.
func ProxyLink(host string, port int, cover, secret string) (tg, https string) {
	cs, err := telemt.ClientSecret(telemt.ModeTLS, secret, cover)
	if err != nil {
		cs = "ee" + secret + hex.EncodeToString([]byte(cover))
	}
	q := "server=" + url.QueryEscape(host) + "&port=" + strconv.Itoa(port) + "&secret=" + cs
	return "tg://proxy?" + q, "https://t.me/proxy?" + q
}

// Links is a user's link for the cover site, and one for each extra cover name.
func Links(host string, port int, cover string, extra []string, secret string) LinksView {
	tg, https := ProxyLink(host, port, cover, secret)
	v := LinksView{TG: tg, HTTPS: https, Extra: []ExtraLinkView{}}
	for _, d := range extra {
		t, h := ProxyLink(host, port, d, secret)
		v.Extra = append(v.Extra, ExtraLinkView{Domain: d, TG: t, HTTPS: h})
	}
	return v
}

type UserView struct {
	Name        string     `json:"name"`
	Enabled     bool       `json:"enabled"`
	State       string     `json:"state"`
	MaxIPs      int        `json:"maxIps"`
	ExpiresAt   *time.Time `json:"expiresAt"`
	QuotaBytes  uint64     `json:"quotaBytes"`
	Up          uint64     `json:"up"`
	Down        uint64     `json:"down"`
	CreatedAt   time.Time  `json:"createdAt"`
	Secret      string     `json:"secret"`
	Links       LinksView  `json:"links"`
	Note        string     `json:"note"`
	Online      *bool      `json:"online"` // nil: the proxy can't say, or its last word is stale
	Connections *int       `json:"connections"`
	Devices     *int       `json:"devices"`
	LastSeenAt  *time.Time `json:"lastSeenAt"`
	TodayUp     uint64     `json:"todayUp"`
	TodayDown   uint64     `json:"todayDown"`
	MonthUp     uint64     `json:"monthUp"`
	MonthDown   uint64     `json:"monthDown"`
}

func (h Handlers) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h Handlers) view(p Proxy, u users.User, uu users.UserUsage) UserView {
	v := UserView{
		Name: u.Name, Enabled: u.Enabled, MaxIPs: u.MaxIPs, ExpiresAt: UTC(u.ExpiresAt), QuotaBytes: u.QuotaBytes,
		Up: uu.Up, Down: uu.Down, CreatedAt: u.CreatedAt.UTC(), Secret: u.Secret, Links: p.Links(u.Secret),
		Note: u.Note, LastSeenAt: UTC(uu.LastSeen), State: u.State(uu.Up+uu.Down, h.now()),
		TodayUp: uu.TodayUp, TodayDown: uu.TodayDown, MonthUp: uu.MonthUp, MonthDown: uu.MonthDown,
	}
	if uu.Live {
		on, conns, devs := uu.Connections > 0, uu.Connections, uu.Devices
		v.Online, v.Connections, v.Devices = &on, &conns, &devs
	}
	return v
}

func (h Handlers) list(w http.ResponseWriter, r *http.Request) {
	p, err := h.Lookup(r)
	if err != nil {
		h.Fail(w, err)
		return
	}
	use := p.Usage()
	out := []UserView{}
	for _, u := range p.Users() {
		out = append(out, h.view(p, u, use.Users[u.Name]))
	}
	WriteJSON(w, http.StatusOK, out)
}

// mutate edits one user, pushes the result, and answers with the user.
func (h Handlers) mutate(w http.ResponseWriter, r *http.Request, code int, fn func(us *[]users.User) (*users.User, error)) {
	p, err := h.Lookup(r)
	if err != nil {
		h.Fail(w, err)
		return
	}
	var out users.User
	err = p.Edit(func(us *[]users.User, _ map[string]users.Usage) error {
		u, err := fn(us)
		if err != nil {
			return err
		}
		out = *u
		return nil
	})
	if err != nil {
		h.Fail(w, err)
		return
	}
	p.Changed()
	if p, err = h.Lookup(r); err != nil {
		h.Fail(w, err)
		return
	}
	WriteJSON(w, code, h.view(p, out, p.Usage().Users[out.Name]))
}

func find(us []users.User, name string) *users.User {
	for i := range us {
		if us[i].Name == name {
			return &us[i]
		}
	}
	return nil
}

func (h Handlers) create(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name       string     `json:"name"`
		Enabled    *bool      `json:"enabled"`
		MaxIPs     int        `json:"maxIps"`
		ExpiresAt  *time.Time `json:"expiresAt"`
		QuotaBytes uint64     `json:"quotaBytes"`
		Note       string     `json:"note"`
		Secret     string     `json:"secret"` // keep someone's existing secret instead of making one
	}
	if err := Decode(w, r, &b); err != nil {
		h.Fail(w, err)
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if !users.ValidName(b.Name) {
		h.Fail(w, Bad("a user name is 1 to 32 letters, digits, '.', '_' or '-'"))
		return
	}
	if b.MaxIPs < 0 {
		h.Fail(w, Bad("max IPs cannot be negative"))
		return
	}
	note, err := users.CleanNote(b.Note)
	if err != nil {
		h.Fail(w, err)
		return
	}
	var sec string
	if strings.TrimSpace(b.Secret) != "" {
		if sec, err = telemt.NormalizeSecret(b.Secret); err != nil {
			h.Fail(w, Bad("that secret is not usable: give 32 hex characters, or a dd or ee secret"))
			return
		}
	} else if sec, err = telemt.GenerateSecret(); err != nil {
		h.Fail(w, err)
		return
	}
	u := users.User{Name: b.Name, Secret: sec, Enabled: b.Enabled == nil || *b.Enabled, MaxIPs: b.MaxIPs, QuotaBytes: b.QuotaBytes, Note: note, CreatedAt: h.now().UTC()}
	if b.ExpiresAt != nil {
		u.ExpiresAt = b.ExpiresAt.UTC()
	}
	h.mutate(w, r, http.StatusCreated, func(us *[]users.User) (*users.User, error) {
		if find(*us, u.Name) != nil {
			return nil, Conflict("there is already a user called %s", u.Name)
		}
		// two users on one secret could not be told apart
		if slices.ContainsFunc(*us, func(o users.User) bool { return o.Secret == u.Secret }) {
			return nil, Conflict("another user on %s already has that secret", h.Here)
		}
		if len(*us) >= users.MaxUsers {
			return nil, Conflict("%s holds at most %d users", h.Here, users.MaxUsers)
		}
		*us = append(*us, u)
		return &(*us)[len(*us)-1], nil
	})
}

func (h Handlers) update(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Enabled    *bool           `json:"enabled"`
		MaxIPs     *int            `json:"maxIps"`
		ExpiresAt  json.RawMessage `json:"expiresAt"` // null clears, absent keeps
		QuotaBytes *uint64         `json:"quotaBytes"`
		Note       *string         `json:"note"`
	}
	if err := Decode(w, r, &b); err != nil {
		h.Fail(w, err)
		return
	}
	var note string
	if b.Note != nil {
		var err error
		if note, err = users.CleanNote(*b.Note); err != nil {
			h.Fail(w, err)
			return
		}
	}
	if b.MaxIPs != nil && *b.MaxIPs < 0 {
		h.Fail(w, Bad("max IPs cannot be negative"))
		return
	}
	var exp *time.Time
	if len(b.ExpiresAt) > 0 && string(b.ExpiresAt) != "null" {
		var t time.Time
		if err := json.Unmarshal(b.ExpiresAt, &t); err != nil {
			h.Fail(w, Bad("expiresAt must be an RFC 3339 time or null"))
			return
		}
		exp = &t
	}
	name := r.PathValue("name")
	h.mutate(w, r, http.StatusOK, func(us *[]users.User) (*users.User, error) {
		u := find(*us, name)
		if u == nil {
			return nil, ErrNoUser
		}
		if b.Enabled != nil {
			u.Enabled = *b.Enabled
		}
		if b.MaxIPs != nil {
			u.MaxIPs = *b.MaxIPs
		}
		if b.QuotaBytes != nil {
			u.QuotaBytes = *b.QuotaBytes
		}
		if b.Note != nil {
			u.Note = note
		}
		switch {
		case exp != nil:
			u.ExpiresAt = exp.UTC()
		case len(b.ExpiresAt) > 0:
			u.ExpiresAt = time.Time{}
		}
		return u, nil
	})
}

func (h Handlers) remove(w http.ResponseWriter, r *http.Request) {
	p, err := h.Lookup(r)
	if err != nil {
		h.Fail(w, err)
		return
	}
	name := r.PathValue("name")
	err = p.Edit(func(us *[]users.User, base map[string]users.Usage) error {
		if find(*us, name) == nil {
			return ErrNoUser
		}
		*us = slices.DeleteFunc(*us, func(u users.User) bool { return u.Name == name })
		delete(base, name)
		return nil
	})
	if err != nil {
		h.Fail(w, err)
		return
	}
	p.Forget(name)
	p.Changed()
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h Handlers) rotateSecret(w http.ResponseWriter, r *http.Request) {
	sec, err := telemt.GenerateSecret()
	if err != nil {
		h.Fail(w, err)
		return
	}
	name := r.PathValue("name")
	h.mutate(w, r, http.StatusOK, func(us *[]users.User) (*users.User, error) {
		u := find(*us, name)
		if u == nil {
			return nil, ErrNoUser
		}
		u.Secret = sec
		return u, nil
	})
}

func (h Handlers) resetUsage(w http.ResponseWriter, r *http.Request) {
	// what memory still holds belongs to before the reset
	if p, err := h.Lookup(r); err == nil {
		if err := p.Flush(); err != nil {
			h.Fail(w, err)
			return
		}
	}
	name := r.PathValue("name")
	h.mutate(w, r, http.StatusOK, func(us *[]users.User) (*users.User, error) {
		u := find(*us, name)
		if u == nil {
			return nil, ErrNoUser
		}
		u.Up, u.Down = 0, 0
		return u, nil
	})
}

// bulk acts on several users at once, with one push to the proxy.
func (h Handlers) bulk(w http.ResponseWriter, r *http.Request) {
	p, err := h.Lookup(r)
	if err != nil {
		h.Fail(w, err)
		return
	}
	var b struct {
		Action string   `json:"action"`
		Names  []string `json:"names"`
		Days   int      `json:"days"`
	}
	if err := Decode(w, r, &b); err != nil {
		h.Fail(w, err)
		return
	}
	if len(b.Names) == 0 || len(b.Names) > users.MaxUsers {
		h.Fail(w, Bad("pick the users to change"))
		return
	}
	if b.Days == 0 {
		b.Days = 30
	}
	if b.Days < 1 || b.Days > 3650 {
		h.Fail(w, Bad("renew by 1 to 3650 days"))
		return
	}
	picked := map[string]bool{}
	for _, n := range b.Names {
		picked[n] = true
	}
	now := h.now()
	changed := 0
	err = p.Edit(func(us *[]users.User, base map[string]users.Usage) error {
		switch b.Action {
		case "pause", "resume":
			for i := range *us {
				if u := &(*us)[i]; picked[u.Name] && u.Enabled != (b.Action == "resume") {
					u.Enabled = b.Action == "resume"
					changed++
				}
			}
		case "renew":
			for i := range *us {
				// a user who never expires has nothing to renew
				if u := &(*us)[i]; picked[u.Name] && !u.ExpiresAt.IsZero() {
					u.ExpiresAt = users.Renewed(u.ExpiresAt, now, b.Days)
					changed++
				}
			}
		case "delete":
			before := len(*us)
			*us = slices.DeleteFunc(*us, func(u users.User) bool { return picked[u.Name] })
			for n := range picked {
				delete(base, n)
			}
			changed = before - len(*us)
		default:
			return Bad("the action is pause, resume, renew or delete")
		}
		if changed == 0 {
			return errUnchanged
		}
		return nil
	})
	if errors.Is(err, errUnchanged) {
		WriteJSON(w, http.StatusOK, map[string]any{"changed": 0})
		return
	}
	if err != nil {
		h.Fail(w, err)
		return
	}
	if b.Action == "delete" {
		for n := range picked {
			p.Forget(n)
		}
	}
	p.Changed()
	WriteJSON(w, http.StatusOK, map[string]any{"changed": changed})
}
