package model

import (
	"fmt"
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
}

type NewUserSettings struct {
	PasswordLength        int  `json:"password_length"`
	ChangePasswordAtLogon bool `json:"change_password_at_logon"`
	Enabled               bool `json:"enabled"`
}

type Settings struct {
	Schedule ScheduleSettings `json:"schedule"`
	Approval ApprovalSettings `json:"approval"`
	Engine   EngineSettings   `json:"engine"`
	AD       ADSettings       `json:"ad"`
	Naming   NamingSettings   `json:"naming"`
	NewUser  NewUserSettings  `json:"new_user"`
	Notify   NotifySettings   `json:"notifications"`
}

// DefaultSettings are deliberately conservative: dry-run on, everything manual.
func DefaultSettings() Settings {
	return Settings{
		Schedule: ScheduleSettings{Enabled: false, Kind: "interval", IntervalMinutes: 60, DailyTime: "02:00", CronExpr: "0 */4 * * *", Timezone: "UTC"},
		Approval: ApprovalSettings{Create: "manual", Update: "manual", Disable: "manual", Enable: "manual", Delete: "manual"},
		Engine:   EngineSettings{DryRun: true, MaxDestructivePerRun: 10, DeleteAfterDays: 30, RemoveStaleGroups: true, MaxDeletesPerRun: 5, MaxDeletesPerDay: 20},
		AD:       ADSettings{IDAttribute: "employeeID", StripGroupsOnDisable: true},
		Naming: NamingSettings{
			SamTemplate:   `{{ if and .FirstName .LastName }}{{ first .FirstName 1 | lower }}{{ .LastName | lower | alnum }}{{ end }}`,
			UPNTemplate:   `{{ if and .FirstName .LastName }}{{ .FirstName | lower | alnum }}.{{ .LastName | lower | alnum }}{{ end }}`,
			CNTemplate:    `{{ if and .FirstName .LastName }}{{ .FirstName }} {{ .LastName }}{{ end }}`,
			MaxCandidates: 9,
		},
		NewUser: NewUserSettings{PasswordLength: 20, ChangePasswordAtLogon: true, Enabled: true},
		Notify: NotifySettings{
			Provider: NotifySMTP, SubjectPrefix: "[IDLifecycle]", To: []string{},
			SMTP:   SMTPSettings{Port: 587, Security: "starttls", Auth: "none"},
			Events: NotifyEvents{RunFailed: true, ApprovalsPending: true, ChangesFailed: true, Threshold: true},
		},
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

	for name, v := range map[string]string{
		"create": s.Approval.Create, "update": s.Approval.Update, "disable": s.Approval.Disable,
		"enable": s.Approval.Enable, "delete": s.Approval.Delete,
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
	if s.NewUser.PasswordLength < 14 || s.NewUser.PasswordLength > 128 {
		return fmt.Errorf("new_user.password_length must be between 14 and 128")
	}
	return nil
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
