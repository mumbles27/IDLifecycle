package model

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

type ScheduleSettings struct {
	Enabled         bool   `json:"enabled"`
	Kind            string `json:"kind"` // interval | daily | cron
	IntervalMinutes int    `json:"interval_minutes"`
	DailyTime       string `json:"daily_time"` // HH:MM
	CronExpr        string `json:"cron_expr"`
	Timezone        string `json:"timezone"`
}

type ApprovalSettings struct {
	Create  string `json:"create"`
	Update  string `json:"update"`
	Disable string `json:"disable"`
	Enable  string `json:"enable"`
	Delete  string `json:"delete"`
	// Link: a full, unambiguous correlation match (other links always wait for a person).
	Link string `json:"link"`
}

// For returns the approval mode ("manual" | "auto") for a change type.
func (a ApprovalSettings) For(changeType string) string {
	switch changeType {
	case ChangeCreate:
		return a.Create
	case ChangeUpdate:
		return a.Update
	case ChangeDisable:
		return a.Disable
	case ChangeEnable:
		return a.Enable
	case ChangeDelete:
		return a.Delete
	case ChangeLink:
		return a.Link
	}
	return "manual"
}

type EngineSettings struct {
	DryRun               bool `json:"dry_run"`
	MaxDestructivePerRun int  `json:"max_destructive_per_run"`
	DeleteAfterDays      int  `json:"delete_after_days"`
	RemoveStaleGroups    bool `json:"remove_stale_groups"`
	EnforceOU            bool `json:"enforce_ou"`
	// MaxDeletesPerRun: when one run plans more deletes than this, every delete of that run is held for
	// manual approval (even when approval.delete is auto). 0 = no limit.
	MaxDeletesPerRun int `json:"max_deletes_per_run"`
	// MaxDeletesPerDay: hard cap on deletes the executor completes in any rolling 24 hours. Approved deletes
	// beyond it stay approved and are applied once the window allows. 0 = no limit.
	MaxDeletesPerDay int `json:"max_deletes_per_day"`
}

// Notification providers.
const (
	NotifySMTP      = "smtp"       // any SMTP server (on-prem Exchange relay, SMTP2GO, ...)
	NotifyM365SMTP  = "m365_smtp"  // Exchange Online SMTP AUTH (smtp.office365.com:587, STARTTLS, OAuth2 XOAUTH2 or basic)
	NotifyM365Graph = "m365_graph" // Microsoft Graph /users/{from}/sendMail with app-only OAuth2 (Mail.Send)
)

// Notification event kinds.
const (
	EventRunFailed        = "run_failed"        // a sync run finished with status error/aborted
	EventApprovalsPending = "approvals_pending" // a run created changes that wait for a human
	EventChangesFailed    = "changes_failed"    // one or more changes failed when applied to AD
	EventThreshold        = "threshold"         // a safety threshold held or deferred changes (bulk guard, delete limits)
	EventRunSummary       = "run_summary"       // every run that planned or applied something
	EventIdentityIssues   = "identity_issues"   // naming conflicts, matches to verify, AD accounts no source person matches
	EventTest             = "test"              // the "send test message" button (always sent)
)

type SMTPSettings struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"` // starttls | tls | none
	Auth     string `json:"auth"`     // none | login | oauth2 (oauth2 = M365 client credentials, XOAUTH2)
	Username string `json:"username"`
	Password string `json:"password"` // secret: encrypted at rest, masked in API responses
}

// OAuthSettings are the Entra ID app registration used for m365_smtp (auth=oauth2) and m365_graph.
type OAuthSettings struct {
	TenantID     string `json:"tenant_id"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"` // secret: encrypted at rest, masked in API responses
}

type NotifyEvents struct {
	RunFailed        bool `json:"run_failed"`
	ApprovalsPending bool `json:"approvals_pending"`
	ChangesFailed    bool `json:"changes_failed"`
	Threshold        bool `json:"threshold"`
	RunSummary       bool `json:"run_summary"`
	IdentityIssues   bool `json:"identity_issues"`
}

// On reports whether an event kind is switched on (test messages always are).
func (e NotifyEvents) On(kind string) bool {
	switch kind {
	case EventRunFailed:
		return e.RunFailed
	case EventApprovalsPending:
		return e.ApprovalsPending
	case EventChangesFailed:
		return e.ChangesFailed
	case EventThreshold:
		return e.Threshold
	case EventRunSummary:
		return e.RunSummary
	case EventIdentityIssues:
		return e.IdentityIssues
	case EventTest:
		return true
	}
	return false
}

type NotifySettings struct {
	Enabled       bool          `json:"enabled"`
	Provider      string        `json:"provider"` // smtp | m365_smtp | m365_graph
	From          string        `json:"from"`
	To            []string      `json:"to"`
	SubjectPrefix string        `json:"subject_prefix"`
	GUIBaseURL    string        `json:"gui_base_url"` // optional link back to the GUI in messages
	SMTP          SMTPSettings  `json:"smtp"`
	OAuth         OAuthSettings `json:"oauth"`
	Events        NotifyEvents  `json:"events"`
}

// Notification is one message the engine wants to send. Lines are plain text (no HTML).
type Notification struct {
	Event   string   `json:"event"`
	Subject string   `json:"subject"`
	Lines   []string `json:"lines"`
	RunID   string   `json:"run_id,omitempty"`
}

type ADSettings struct {
	Server               string   `json:"server"`
	SearchBases          []string `json:"search_bases"`
	DefaultOU            string   `json:"default_ou"`
	DisabledOU           string   `json:"disabled_ou"`
	IDAttribute          string   `json:"id_attribute"`
	UPNSuffix            string   `json:"upn_suffix"`
	StripGroupsOnDisable bool     `json:"strip_groups_on_disable"`
}

type NamingSettings struct {
	SamTemplate   string `json:"sam_template"`
	UPNTemplate   string `json:"upn_template"`
	CNTemplate    string `json:"cn_template"`
	MaxCandidates int    `json:"max_candidates"`
	// MailTemplate (optional) renders the local part of a new account's mail address; it gets the same
	// collision number as the sAMAccountName/UPN. Set at creation only. MailDomain defaults to ad.upn_suffix.
	MailTemplate string `json:"mail_template"`
	MailDomain   string `json:"mail_domain"`
	// ConflictMode decides what happens when a generated name (sAMAccountName, UPN, mail, CN) is taken.
	ConflictMode         string `json:"conflict_mode"`
	ConflictRandomDigits int    `json:"conflict_random_digits"`   // random mode
	ConflictSuffix       string `json:"conflict_suffix_template"` // template mode, e.g. {{ last .EmployeeID 4 }}
}

// Naming conflict modes (NamingSettings.ConflictMode).
const (
	ConflictManual   = "manual"   // only the template's names; taken -> wait for a person to choose (default)
	ConflictNumber   = "number"   // jdoe, jdoe2, jdoe3 ... up to max_candidates
	ConflictRandom   = "random"   // jdoe, then jdoe + random digits (stable per person)
	ConflictTemplate = "template" // jdoe, then jdoe + conflict_suffix_template
)

// Correlation compare kinds (CorrelationRule.Compare).
const (
	CompareExact = "exact" // equal after trimming
	CompareCI    = "ci"    // case-insensitive, inner spaces collapsed
	CompareName  = "name"  // case, accents, punctuation and spaces ignored (O'Brien = obrien, José = jose)
)

// Rename on link (CorrelationSettings.Rename).
const (
	RenameNever       = "never"
	RenameTransitions = "transitions" // only when the account belonged to another person record (contractor -> employee)
	RenameAlways      = "always"
)

// What a disable does with the account's groups (LifecycleGroups.DisabledRemove).
const (
	DisabledRemoveAllExcept = "all_except" // every group except disabled_keep and disabled_add (privileged groups are kept by the script)
	DisabledRemoveManaged   = "managed"    // only groups this service manages (role groups, active_add, disabled_add)
	DisabledRemoveNone      = "none"       // no group is removed
)

// LifecycleGroups are groups tied to an account's lifecycle state rather than to a role. Lists hold group identities as
// typed (sAMAccountName or DN), like role grants.
type LifecycleGroups struct {
	ActiveAdd      []string `json:"active_add"`      // every active account is a member (create, update, enable)
	DisabledAdd    []string `json:"disabled_add"`    // added on disable, removed again on enable
	DisabledRemove string   `json:"disabled_remove"` // all_except | managed | none; "" = derived from ad.strip_groups_on_disable
	DisabledKeep   []string `json:"disabled_keep"`   // never removed on disable
}

// ApplyDefaults fills values derived from older settings documents (call after loading a stored document).
func (s *Settings) ApplyDefaults() {
	l := &s.Lifecycle
	if l.DisabledRemove == "" {
		l.DisabledRemove = DisabledRemoveNone
		if s.AD.StripGroupsOnDisable {
			l.DisabledRemove = DisabledRemoveAllExcept
		}
	}
	for _, p := range []*[]string{&l.ActiveAdd, &l.DisabledAdd, &l.DisabledKeep} {
		if *p == nil {
			*p = []string{}
		}
	}
}

func (l *LifecycleGroups) validate() error {
	switch l.DisabledRemove {
	case DisabledRemoveAllExcept, DisabledRemoveManaged, DisabledRemoveNone:
	default:
		return fmt.Errorf("lifecycle_groups.disabled_remove must be all_except, managed or none")
	}
	for name, p := range map[string]*[]string{"active_add": &l.ActiveAdd, "disabled_add": &l.DisabledAdd, "disabled_keep": &l.DisabledKeep} {
		clean := []string{}
		seen := map[string]bool{}
		for _, g := range *p {
			g = strings.TrimSpace(g)
			if g == "" || seen[strings.ToLower(g)] {
				continue
			}
			if len(g) > 512 || strings.ContainsAny(g, "\r\n") {
				return fmt.Errorf("lifecycle_groups.%s: %q is not a group name or DN", name, g)
			}
			seen[strings.ToLower(g)] = true
			clean = append(clean, g)
		}
		if len(clean) > 100 {
			return fmt.Errorf("lifecycle_groups.%s: at most 100 groups", name)
		}
		*p = clean
	}
	return nil
}

// CorrelationRule compares one source field with one AD attribute.
type CorrelationRule struct {
	Field    string `json:"field"`
	ADAttr   string `json:"ad_attr"`
	Compare  string `json:"compare"`
	Required bool   `json:"required"`
}

// CorrelationSettings match people without an AD account (by id attribute) to existing accounts: accounts that have no
// id yet (first rollout), or that belong to another source's record (a contractor who is now an employee) or to a
// former record of the same source (a rehire with a new id).
type CorrelationSettings struct {
	Enabled            bool              `json:"enabled"`
	Rules              []CorrelationRule `json:"rules"`
	Rename             string            `json:"rename"`
	MaxAutoLinksPerRun int               `json:"max_auto_links_per_run"` // 0 = no limit
	ReportUncorrelated bool              `json:"report_uncorrelated"`
	IgnoreAccounts     []string          `json:"ignore_accounts"` // sAMAccountName wildcards or DN suffixes
}

// Initial password modes (NewUserSettings.PasswordMode).
const (
	PasswordModeRandom   = "random"   // crypto-random, PasswordLength characters
	PasswordModeTemplate = "template" // rendered from PasswordTemplate per account; change at logon is forced
)

type NewUserSettings struct {
	PasswordMode          string `json:"password_mode"`
	PasswordLength        int    `json:"password_length"`
	PasswordTemplate      string `json:"password_template"`
	ChangePasswordAtLogon bool   `json:"change_password_at_logon"`
	Enabled               bool   `json:"enabled"`
}

// Local sign-in modes (AuthSettings.LocalLogin).
const (
	LocalLoginAll        = "all"         // every local account may sign in with its password
	LocalLoginAdminsOnly = "admins_only" // only local admins (break-glass); everyone else uses OIDC
)

// AuthSettings controls how people sign in to the GUI. Local accounts always exist (the CLI creates them);
// OIDC (Microsoft Entra ID) is optional.
type AuthSettings struct {
	LocalLogin string       `json:"local_login"`
	OIDC       OIDCSettings `json:"oidc"`
}

// OIDCSettings is a Microsoft Entra ID app registration used for sign-in (authorization code flow with PKCE).
// A user's role is the highest one whose list contains one of the user's app roles or group object IDs.
type OIDCSettings struct {
	Enabled        bool     `json:"enabled"`
	TenantID       string   `json:"tenant_id"`     // directory (tenant) ID, a GUID
	ClientID       string   `json:"client_id"`     // application (client) ID, a GUID
	ClientSecret   string   `json:"client_secret"` // encrypted at rest, masked in the API
	RedirectURL    string   `json:"redirect_url"`  // https://<host>[:port]/api/auth/oidc/callback, registered in Entra
	ButtonLabel    string   `json:"button_label"`
	AdminValues    []string `json:"admin_values"`
	ApproverValues []string `json:"approver_values"`
	ViewerValues   []string `json:"viewer_values"`
}

// Placement of accounts without a mapping (PlacementSettings.Unmapped).
const (
	UnmappedHold    = "hold"    // the create/move waits in Approvals until someone chooses the OU
	UnmappedDefault = "default" // use ad.default_ou (with a warning)
)

// PlacementRule maps a value of the placement field (e.g. a department) to an OU. Match is compared without
// regard to case or surrounding spaces; '*' matches any run of characters (Sales* = every Sales department).
type PlacementRule struct {
	Match string `json:"match"`
	OU    string `json:"ou"`
}

// PlacementSettings chooses a new account's OU from a source field (OU placement). Precedence: an OU a person chose
// for the identity, then a role rule's OU, then these rules, then Unmapped.
type PlacementSettings struct {
	Enabled  bool   `json:"enabled"`
	Field    string `json:"field"` // source field, e.g. Department
	Unmapped string `json:"unmapped"`
	// MoveOnChange moves an existing account when its field value changes (a department move), and only then.
	MoveOnChange bool            `json:"move_on_change"`
	Rules        []PlacementRule `json:"rules"`
}

// BackupSettings controls the service's scheduled backups (encrypted .idlbak files, see internal/backup).
type BackupSettings struct {
	Enabled       bool   `json:"enabled"`
	IntervalHours int    `json:"interval_hours"`
	Folder        string `json:"folder"` // empty = <data_dir>\backups
	Keep          int    `json:"keep"`
	Passphrase    string `json:"passphrase"` // secret: encrypted at rest, masked in API responses
}

type Settings struct {
	Schedule  ScheduleSettings    `json:"schedule"`
	Approval  ApprovalSettings    `json:"approval"`
	Engine    EngineSettings      `json:"engine"`
	AD        ADSettings          `json:"ad"`
	Naming    NamingSettings      `json:"naming"`
	NewUser   NewUserSettings     `json:"new_user"`
	Notify    NotifySettings      `json:"notifications"`
	Auth      AuthSettings        `json:"auth"`
	Backup    BackupSettings      `json:"backup"`
	Placement PlacementSettings   `json:"placement"`
	Correlate CorrelationSettings `json:"correlation"`
	Lifecycle LifecycleGroups     `json:"lifecycle_groups"`
}

// DefaultSettings are deliberately conservative: dry-run on, everything manual.
func DefaultSettings() Settings {
	return Settings{
		Schedule: ScheduleSettings{Enabled: false, Kind: "interval", IntervalMinutes: 60, DailyTime: "02:00", CronExpr: "0 */4 * * *", Timezone: "UTC"},
		Approval: ApprovalSettings{Create: "manual", Update: "manual", Disable: "manual", Enable: "manual", Delete: "manual", Link: "auto"},
		Engine:   EngineSettings{DryRun: true, MaxDestructivePerRun: 10, DeleteAfterDays: 30, RemoveStaleGroups: true, MaxDeletesPerRun: 5, MaxDeletesPerDay: 20},
		AD:       ADSettings{IDAttribute: "employeeID", StripGroupsOnDisable: true},
		Naming: NamingSettings{
			SamTemplate:   `{{ if and .FirstName .LastName }}{{ first .FirstName 1 | lower }}{{ .LastName | lower | alnum }}{{ end }}`,
			UPNTemplate:   `{{ if and .FirstName .LastName }}{{ .FirstName | lower | alnum }}.{{ .LastName | lower | alnum }}{{ end }}`,
			CNTemplate:    `{{ if and .FirstName .LastName }}{{ .FirstName }} {{ .LastName }}{{ end }}`,
			MaxCandidates: 9,
			ConflictMode:  ConflictManual, ConflictRandomDigits: 3, ConflictSuffix: `{{ last .EmployeeID 4 }}`,
		},
		NewUser: NewUserSettings{
			PasswordMode: PasswordModeRandom, PasswordLength: 20, ChangePasswordAtLogon: true, Enabled: true,
			PasswordTemplate: `{{ upper (first .LastName 1) }}{{ lower (first .FirstName 1) }}{{ last .EmployeeID 4 }}!{{ random 6 }}`,
		},
		Notify: NotifySettings{
			Provider: NotifySMTP, SubjectPrefix: "[IDLifecycle]", To: []string{},
			SMTP:   SMTPSettings{Port: 587, Security: "starttls", Auth: "none"},
			Events: NotifyEvents{RunFailed: true, ApprovalsPending: true, ChangesFailed: true, Threshold: true, IdentityIssues: true},
		},
		Auth:      AuthSettings{LocalLogin: LocalLoginAll, OIDC: DefaultOIDC()},
		Backup:    BackupSettings{IntervalHours: 24, Keep: 14},
		Placement: PlacementSettings{Field: "Department", Unmapped: UnmappedHold, MoveOnChange: true, Rules: []PlacementRule{}},
		Correlate: CorrelationSettings{
			Rules: []CorrelationRule{
				{Field: "FirstName", ADAttr: "givenName", Compare: CompareName, Required: true},
				{Field: "LastName", ADAttr: "sn", Compare: CompareName, Required: true},
				{Field: "Department", ADAttr: "department", Compare: CompareCI},
			},
			Rename: RenameTransitions, MaxAutoLinksPerRun: 25, ReportUncorrelated: true, IgnoreAccounts: []string{},
		},
	}
}

// DefaultOIDC is a disabled configuration with the app role values the setup guide tells you to create.
func DefaultOIDC() OIDCSettings {
	return OIDCSettings{
		ButtonLabel:    "Sign in with Microsoft",
		AdminValues:    []string{"IDLifecycle.Admin"},
		ApproverValues: []string{"IDLifecycle.Approver"},
		ViewerValues:   []string{"IDLifecycle.Viewer"},
	}
}

// Validate normalizes and checks a settings document. It mutates s to fill blanks.
func (s *Settings) Validate() error {
	sc := &s.Schedule
	switch sc.Kind {
	case "interval":
		if sc.IntervalMinutes < 1 || sc.IntervalMinutes > 7*24*60 {
			return fmt.Errorf("schedule.interval_minutes must be between 1 and 10080")
		}
	case "daily":
		if _, err := time.Parse("15:04", sc.DailyTime); err != nil {
			return fmt.Errorf("schedule.daily_time must be HH:MM")
		}
	case "cron":
		if _, err := cron.ParseStandard(sc.CronExpr); err != nil {
			return fmt.Errorf("schedule.cron_expr invalid: %v", err)
		}
	default:
		return fmt.Errorf("schedule.kind must be interval, daily or cron")
	}
	if sc.Timezone == "" {
		sc.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(sc.Timezone); err != nil {
		return fmt.Errorf("schedule.timezone %q is not a known time zone", sc.Timezone)
	}

	if s.Approval.Link == "" {
		s.Approval.Link = DefaultSettings().Approval.Link
	}
	for name, v := range map[string]string{
		"create": s.Approval.Create, "update": s.Approval.Update, "disable": s.Approval.Disable,
		"enable": s.Approval.Enable, "delete": s.Approval.Delete, "link": s.Approval.Link,
	} {
		if v != "manual" && v != "auto" {
			return fmt.Errorf("approval.%s must be manual or auto", name)
		}
	}

	if s.Engine.MaxDestructivePerRun < 0 {
		return fmt.Errorf("engine.max_destructive_per_run must be >= 0")
	}
	if s.Engine.DeleteAfterDays < 0 {
		return fmt.Errorf("engine.delete_after_days must be >= 0 (0 disables deletion)")
	}
	if s.Engine.MaxDeletesPerRun < 0 || s.Engine.MaxDeletesPerDay < 0 {
		return fmt.Errorf("engine.max_deletes_per_run and engine.max_deletes_per_day must be >= 0 (0 = no limit)")
	}

	s.AD.IDAttribute = strings.TrimSpace(s.AD.IDAttribute)
	if s.AD.IDAttribute == "" {
		return fmt.Errorf("ad.id_attribute is required")
	}
	cleaned := make([]string, 0, len(s.AD.SearchBases))
	for _, b := range s.AD.SearchBases {
		if b = strings.TrimSpace(b); b != "" {
			cleaned = append(cleaned, b)
		}
	}
	s.AD.SearchBases = cleaned

	if s.Naming.MaxCandidates < 1 || s.Naming.MaxCandidates > 99 {
		return fmt.Errorf("naming.max_candidates must be between 1 and 99")
	}
	if strings.TrimSpace(s.Naming.SamTemplate) == "" || strings.TrimSpace(s.Naming.UPNTemplate) == "" || strings.TrimSpace(s.Naming.CNTemplate) == "" {
		return fmt.Errorf("naming templates must not be empty")
	}
	n := &s.Naming
	n.MailTemplate = strings.TrimSpace(n.MailTemplate)
	n.MailDomain = strings.TrimPrefix(strings.TrimSpace(n.MailDomain), "@")
	if n.MailTemplate != "" {
		domain := n.MailDomain
		if domain == "" {
			domain = strings.TrimSpace(s.AD.UPNSuffix)
		}
		if !domainRe.MatchString(domain) {
			return fmt.Errorf("naming.mail_domain (or ad.upn_suffix) must be a domain such as corp.example.com when naming.mail_template is set")
		}
	}

	switch n.ConflictMode {
	case "":
		n.ConflictMode = ConflictManual
	case ConflictManual, ConflictNumber, ConflictRandom:
	case ConflictTemplate:
		if strings.TrimSpace(n.ConflictSuffix) == "" {
			return fmt.Errorf("naming.conflict_suffix_template is required when naming.conflict_mode is template")
		}
	default:
		return fmt.Errorf("naming.conflict_mode must be manual, number, random or template")
	}
	if n.ConflictRandomDigits == 0 {
		n.ConflictRandomDigits = 3
	}
	if n.ConflictRandomDigits < 2 || n.ConflictRandomDigits > 6 {
		return fmt.Errorf("naming.conflict_random_digits must be between 2 and 6")
	}
	if err := s.Correlate.validate(); err != nil {
		return err
	}
	s.ApplyDefaults()
	if err := s.Lifecycle.validate(); err != nil {
		return err
	}
	// The old switch follows the new mode, for anything that still reads it.
	s.AD.StripGroupsOnDisable = s.Lifecycle.DisabledRemove != DisabledRemoveNone

	pl := &s.Placement
	pl.Field = strings.TrimSpace(pl.Field)
	if pl.Field == "" {
		pl.Field = "Department"
	}
	switch pl.Unmapped {
	case "":
		pl.Unmapped = UnmappedHold
	case UnmappedHold, UnmappedDefault:
	default:
		return fmt.Errorf("placement.unmapped must be hold or default")
	}
	if pl.Rules == nil {
		pl.Rules = []PlacementRule{}
	}
	seen := map[string]bool{}
	for i := range pl.Rules {
		r := &pl.Rules[i]
		r.Match, r.OU = strings.TrimSpace(r.Match), strings.TrimSpace(r.OU)
		if r.Match == "" || r.OU == "" {
			return fmt.Errorf("placement.rules[%d]: both the value and the OU are required", i)
		}
		if !strings.Contains(r.OU, "=") {
			return fmt.Errorf("placement.rules[%d]: %q is not an OU distinguished name (OU=...,DC=...)", i, r.OU)
		}
		key := strings.ToLower(r.Match)
		if seen[key] {
			return fmt.Errorf("placement.rules: %q is listed twice", r.Match)
		}
		seen[key] = true
	}
	if pl.Enabled && pl.Unmapped == UnmappedDefault && strings.TrimSpace(s.AD.DefaultOU) == "" {
		return fmt.Errorf("placement.unmapped = default needs ad.default_ou")
	}

	b := &s.Backup
	if b.IntervalHours == 0 {
		b.IntervalHours = 24
	}
	if b.Keep == 0 {
		b.Keep = 14
	}
	if b.IntervalHours < 1 || b.IntervalHours > 720 {
		return fmt.Errorf("backup.interval_hours must be between 1 and 720")
	}
	if b.Keep < 1 || b.Keep > 1000 {
		return fmt.Errorf("backup.keep must be between 1 and 1000")
	}
	b.Folder = strings.TrimSpace(b.Folder)
	if b.Folder != "" && !filepath.IsAbs(b.Folder) {
		return fmt.Errorf("backup.folder must be a full path such as D:\\Backups\\IDLifecycle (or empty for <data_dir>\\backups)")
	}

	u := &s.NewUser
	if u.PasswordLength < 14 || u.PasswordLength > 128 {
		return fmt.Errorf("new_user.password_length must be between 14 and 128")
	}
	switch u.PasswordMode {
	case "":
		u.PasswordMode = PasswordModeRandom
	case PasswordModeRandom:
	case PasswordModeTemplate:
		if strings.TrimSpace(u.PasswordTemplate) == "" {
			return fmt.Errorf("new_user.password_template is required when new_user.password_mode is template")
		}
		// A password anyone could work out from HR data must be replaced at first sign-in.
		u.ChangePasswordAtLogon = true
	default:
		return fmt.Errorf("new_user.password_mode must be random or template")
	}
	return s.Auth.Validate()
}

var attrNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)

func (c *CorrelationSettings) validate() error {
	switch c.Rename {
	case "":
		c.Rename = RenameTransitions
	case RenameNever, RenameTransitions, RenameAlways:
	default:
		return fmt.Errorf("correlation.rename must be never, transitions or always")
	}
	if c.MaxAutoLinksPerRun < 0 {
		return fmt.Errorf("correlation.max_auto_links_per_run must be >= 0 (0 = no limit)")
	}
	if c.Rules == nil {
		c.Rules = []CorrelationRule{}
	}
	required := 0
	seen := map[string]bool{}
	for i := range c.Rules {
		r := &c.Rules[i]
		r.Field, r.ADAttr = strings.TrimSpace(r.Field), strings.TrimSpace(r.ADAttr)
		if r.Field == "" || r.ADAttr == "" {
			return fmt.Errorf("correlation.rules[%d]: both the source field and the AD attribute are required", i)
		}
		if !attrNameRe.MatchString(r.ADAttr) {
			return fmt.Errorf("correlation.rules[%d]: %q is not an AD attribute name", i, r.ADAttr)
		}
		switch r.Compare {
		case "":
			r.Compare = CompareCI
		case CompareExact, CompareCI, CompareName:
		default:
			return fmt.Errorf("correlation.rules[%d]: compare must be exact, ci or name", i)
		}
		key := strings.ToLower(r.Field) + "\x00" + strings.ToLower(r.ADAttr)
		if seen[key] {
			return fmt.Errorf("correlation.rules: %s / %s is listed twice", r.Field, r.ADAttr)
		}
		seen[key] = true
		if r.Required {
			required++
		}
	}
	if c.Enabled && required == 0 {
		return fmt.Errorf("correlation: mark at least one rule as required (for example the last name), or every account would be a candidate for every person")
	}
	c.IgnoreAccounts = cleanValues(c.IgnoreAccounts)
	return nil
}

var domainRe = regexp.MustCompile(`^(?i:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\.(?i:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?))+$`)

var guidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// OIDCCallbackPath is where Entra ID sends the browser back; the redirect URL must end with it.
const OIDCCallbackPath = "/api/auth/oidc/callback"

// Validate normalizes and checks the sign-in settings (the client secret is checked by the API, which stores it).
func (a *AuthSettings) Validate() error {
	if a.LocalLogin == "" {
		a.LocalLogin = LocalLoginAll
	}
	o := &a.OIDC
	o.TenantID = strings.ToLower(strings.TrimSpace(o.TenantID))
	o.ClientID = strings.ToLower(strings.TrimSpace(o.ClientID))
	o.RedirectURL = strings.TrimSpace(o.RedirectURL)
	if o.ButtonLabel = strings.TrimSpace(o.ButtonLabel); o.ButtonLabel == "" {
		o.ButtonLabel = DefaultOIDC().ButtonLabel
	}
	if len(o.ButtonLabel) > 60 {
		return fmt.Errorf("auth.oidc.button_label must be at most 60 characters")
	}
	for _, l := range []*[]string{&o.AdminValues, &o.ApproverValues, &o.ViewerValues} {
		*l = cleanValues(*l)
	}

	switch a.LocalLogin {
	case LocalLoginAll:
	case LocalLoginAdminsOnly:
		if !o.Enabled {
			return fmt.Errorf("auth.local_login admins_only needs Entra ID sign-in enabled, or nobody but administrators could sign in")
		}
	default:
		return fmt.Errorf("auth.local_login must be all or admins_only")
	}
	if !o.Enabled {
		return nil
	}
	if !guidRe.MatchString(o.TenantID) {
		return fmt.Errorf("auth.oidc.tenant_id must be the directory (tenant) ID, a GUID like 72f988bf-86f1-41af-91ab-2d7cd011db47 (a domain name is not accepted: the token issuer contains the GUID)")
	}
	if !guidRe.MatchString(o.ClientID) {
		return fmt.Errorf("auth.oidc.client_id must be the application (client) ID, a GUID")
	}
	if err := ValidateRedirectURL(o.RedirectURL); err != nil {
		return fmt.Errorf("auth.oidc.redirect_url: %w", err)
	}
	if len(o.AdminValues)+len(o.ApproverValues)+len(o.ViewerValues) == 0 {
		return fmt.Errorf("auth.oidc: list at least one app role value or group object ID for a role, or nobody can sign in with Entra ID")
	}
	return nil
}

// ValidateRedirectURL accepts https://host[:port]/api/auth/oidc/callback, or http only for localhost (what Entra ID allows).
func ValidateRedirectURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must be an absolute URL like https://idl.corp.example.com:8443%s", OIDCCallbackPath)
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1"):
	default:
		return fmt.Errorf("must use https (Entra ID allows http only for localhost)")
	}
	if u.Path != OIDCCallbackPath {
		return fmt.Errorf("the path must be %s", OIDCCallbackPath)
	}
	return nil
}

func cleanValues(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[strings.ToLower(v)] {
			continue
		}
		seen[strings.ToLower(v)] = true
		out = append(out, v)
	}
	return out
}

// ReadyForLive reports why the settings are not sufficient for writing to AD ("" when ready).
func (s Settings) ReadyForLive() string {
	switch {
	case len(s.AD.SearchBases) == 0:
		return "ad.search_bases must list at least one search base"
	case s.AD.DefaultOU == "":
		return "ad.default_ou is required"
	case s.AD.UPNSuffix == "":
		return "ad.upn_suffix is required"
	}
	return ""
}
