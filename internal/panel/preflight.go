package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/botdata"
	"github.com/cruciblelab/crucible-analytic/internal/devgate"
)

// Preflight is the list of things that cannot be done from the panel,
// and the machinery that checks whether they were done.
//
// It closes the setup wizard. Everything before it is a form somebody
// fills in; this is the part that says "these need a shell on the
// server", lists them, and then - the part that matters - actually
// verifies each one rather than asking the installer to confirm they
// remember doing it.
//
// A checklist nobody verifies is a checklist everybody ticks. The whole
// value here is that pressing the button runs real queries, stats real
// directories and makes real requests, so "kuruldu" is an observation
// rather than a claim.
//
// Two of the checks are deliberately *negative*: they assert the panel
// role cannot read the analytics tables and the API role cannot write.
// Those are the isolation the entire design rests on, and a deployment
// where somebody granted a bit too much would otherwise look perfectly
// healthy right up until it mattered.

// CheckStatus is the outcome of one check.
type CheckStatus string

const (
	// CheckPass means the check ran and the deployment is correct.
	CheckPass CheckStatus = "pass"
	// CheckFail means the check ran and found something wrong.
	CheckFail CheckStatus = "fail"
	// CheckWarn means something worth knowing that does not block
	// handover.
	CheckWarn CheckStatus = "warn"
	// CheckSkip means the check could not run at all - usually because
	// it was not configured. Deliberately distinct from pass: "we looked
	// and it was fine" and "we did not look" are different facts, the
	// same distinction this project keeps everywhere else.
	CheckSkip CheckStatus = "skip"
)

// CheckSeverity says whether a failure blocks completing the wizard.
type CheckSeverity string

const (
	// SeverityRequired blocks handover. The deployment does not work, or
	// does not work safely, without it.
	SeverityRequired CheckSeverity = "required"
	// SeverityRecommended is shown and does not block. Someone may have
	// a good reason.
	SeverityRecommended CheckSeverity = "recommended"
)

// CheckResult is one line of the final list.
type CheckResult struct {
	ID       string        `json:"id"`
	Label    string        `json:"label"`
	Severity CheckSeverity `json:"severity"`
	Status   CheckStatus   `json:"status"`
	// Detail says what was actually found, in Turkish. Never just
	// "failed": the installer is standing at a terminal and needs to know
	// what to look at.
	Detail string `json:"detail"`
	// Fix is the exact command to run on the server. Copy-pasteable, or
	// it is documentation rather than help.
	Fix string `json:"fix,omitempty"`
}

// PreflightConfig tells the checks where to look.
//
// Everything is optional: an unset field turns its checks into CheckSkip
// rather than CheckFail, because "the installer did not tell us where
// the logs are" is not the same as "the logs are broken".
type PreflightConfig struct {
	// LogDir is the root of the log tree.
	LogDir string
	// DataDir is any path on the volume the database writes to, for the
	// free-space check.
	DataDir string
	// BotDataPath is the known-bot fingerprint file the collector reads.
	// Empty turns its check into a skip rather than a failure - see the
	// rule at the top of this struct.
	BotDataPath string
	// ServiceURLs maps a service name to its /healthz.
	ServiceURLs map[string]string
	// Roles names the database roles to check grants for.
	Roles PreflightRoles
	// MinFreeBytes is the free space below which the disk check fails.
	// Zero takes DefaultMinFreeBytes.
	MinFreeBytes uint64
	// HTTPClient makes the health requests. Nil uses a client with a
	// short timeout - a wizard must not hang because a service is
	// wedged rather than merely down.
	HTTPClient *http.Client
	// DeveloperGate is the gate guarding the settings with legal weight.
	// Nil skips its check rather than reporting it missing: "the wizard
	// was not told" and "there is no developer password" are different
	// facts and must not print the same line.
	DeveloperGate *devgate.Gate
	// Now supplies the clock, for tests.
	Now func() time.Time
}

// PreflightRoles are the database roles a full deployment has.
type PreflightRoles struct {
	Collector string
	Beacon    string
	API       string
	Panel     string
}

// DefaultMinFreeBytes is the free-space floor. Two gigabytes is not
// generous; it is the point at which a full disk stops being
// hypothetical, and a full disk stops the collector - an analytics
// feature taking down the traffic path.
const DefaultMinFreeBytes = 2 << 30

// RunPreflight runs every check and returns the results, worst first so
// the thing needing attention is at the top of the list.
func (s *Store) RunPreflight(ctx context.Context, cfg PreflightConfig) []CheckResult {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if cfg.MinFreeBytes == 0 {
		cfg.MinFreeBytes = DefaultMinFreeBytes
	}

	results := []CheckResult{
		s.checkPanelSchema(ctx),
		s.checkAnalyticsSchema(ctx),
		s.checkSelfMigratingColumns(ctx),
		s.checkSettingsGrant(ctx, cfg.Roles),
		s.checkPanelIsolation(ctx, cfg.Roles),
		s.checkAPIIsReadOnly(ctx, cfg.Roles),
		s.checkRetentionPolicies(ctx),
		s.checkConfiguredRolesExist(ctx, cfg.Roles),
		checkDeveloperPassword(cfg.DeveloperGate),
		s.checkIPTokenKey(),
		checkBotData(cfg.BotDataPath, cfg.Now),
		checkLogDir(cfg.LogDir),
		checkFreeSpace(map[string]string{"veri": cfg.DataDir, "kayıt": cfg.LogDir}, cfg.MinFreeBytes),
		checkBackups(),
	}
	for _, name := range sortedKeys(cfg.ServiceURLs) {
		results = append(results, checkService(ctx, cfg.HTTPClient, name, cfg.ServiceURLs[name]))
	}

	// Worst first: fail, then warn, then skip, then pass. An installer
	// reading top-down should meet the problem before the reassurance.
	rank := map[CheckStatus]int{CheckFail: 0, CheckWarn: 1, CheckSkip: 2, CheckPass: 3}
	sort.SliceStable(results, func(i, j int) bool { return rank[results[i].Status] < rank[results[j].Status] })
	return results
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PreflightComplete reports whether handover may proceed: every required
// check passed.
//
// A recommended check that failed does not block. Somebody may have a
// reason, and a wizard that cannot be finished is a wizard people work
// around.
func PreflightComplete(results []CheckResult) (bool, []CheckResult) {
	blocking := []CheckResult{}
	for _, r := range results {
		if r.Severity == SeverityRequired && r.Status != CheckPass {
			blocking = append(blocking, r)
		}
	}
	return len(blocking) == 0, blocking
}

// --- database checks ---

func (s *Store) checkPanelSchema(ctx context.Context) CheckResult {
	result := CheckResult{
		ID: "schema.panel", Severity: SeverityRequired,
		Label: "Panel tabloları uygulandı mı",
		Fix:   `psql "$DSN" -f internal/panel/schema.sql`,
	}
	want := []string{
		"panel_users", "panel_sessions", "panel_site_members", "panel_audit_log",
		"panel_api_tokens", "panel_dev_access", "panel_login_attempts", "panel_settings",
	}
	missing, err := s.missingTables(ctx, want)
	if err != nil {
		result.Status, result.Detail = CheckFail, "Tablolar sorgulanamadı: "+err.Error()
		return result
	}
	if len(missing) > 0 {
		result.Status = CheckFail
		result.Detail = "Eksik tablo: " + strings.Join(missing, ", ")
		return result
	}
	result.Status, result.Detail = CheckPass, fmt.Sprintf("%d tablonun hepsi yerinde.", len(want))
	return result
}

func (s *Store) checkAnalyticsSchema(ctx context.Context) CheckResult {
	result := CheckResult{
		ID: "schema.analytics", Severity: SeverityRequired,
		Label: "Analitik tabloları ve hypertable'lar",
		Fix:   `psql "$DSN" -f internal/storage/schema.sql && psql "$DSN" -f internal/beacon/schema.sql`,
	}
	missing, err := s.missingTables(ctx, []string{"traffic_snapshots", "beacon_events"})
	if err != nil {
		result.Status, result.Detail = CheckFail, "Tablolar sorgulanamadı: "+err.Error()
		return result
	}
	if len(missing) > 0 {
		result.Status, result.Detail = CheckFail, "Eksik tablo: "+strings.Join(missing, ", ")
		return result
	}

	// A plain table where a hypertable was expected still accepts writes
	// and silently loses every reason TimescaleDB is here: chunk-level
	// retention, compression, and time-ordered scans.
	var hypertables int
	err = s.pool.QueryRow(ctx, `
		SELECT count(*) FROM timescaledb_information.hypertables
		WHERE hypertable_name IN ('traffic_snapshots', 'beacon_events')`).Scan(&hypertables)
	if err != nil {
		result.Status, result.Detail = CheckWarn, "TimescaleDB bilgisi okunamadı: "+err.Error()
		return result
	}
	if hypertables < 2 {
		result.Status = CheckFail
		result.Detail = fmt.Sprintf("Tablolar var ama %d/2 tanesi hypertable. Saklama ve sıkıştırma çalışmaz.", hypertables)
		return result
	}
	result.Status, result.Detail = CheckPass, "İki tablo da hypertable."
	return result
}

// checkSelfMigratingColumns catches the failure this project has already
// had once: CREATE TABLE IF NOT EXISTS does nothing to a table that
// already exists, so a column added to a schema file reaches an existing
// deployment only if somebody re-ran the file.
func (s *Store) checkSelfMigratingColumns(ctx context.Context) CheckResult {
	result := CheckResult{
		ID: "schema.columns", Severity: SeverityRequired,
		Label: "Şema dosyaları yeniden uygulandı mı (yeni sütunlar)",
		Fix:   `psql "$DSN" -f internal/beacon/schema.sql && psql "$DSN" -f internal/panel/schema.sql`,
	}
	expected := map[string][]string{
		"beacon_events": {"utm_source", "utm_medium", "utm_campaign", "click_source", "click_id"},
		"panel_users":   {"totp_last_step"},
	}
	var missing []string
	for table, columns := range expected {
		for _, column := range columns {
			var exists bool
			err := s.pool.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM information_schema.columns
					WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
				table, column).Scan(&exists)
			if err != nil {
				result.Status, result.Detail = CheckFail, "Sütunlar sorgulanamadı: "+err.Error()
				return result
			}
			if !exists {
				missing = append(missing, table+"."+column)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		result.Status = CheckFail
		result.Detail = "Eksik sütun: " + strings.Join(missing, ", ") +
			". Şema dosyaları kendi kendine göç eder; yeniden uygulamak yeterli."
		return result
	}
	result.Status, result.Detail = CheckPass, "Beklenen bütün sütunlar mevcut."
	return result
}

func (s *Store) checkSettingsGrant(ctx context.Context, roles PreflightRoles) CheckResult {
	result := CheckResult{
		ID: "grants.live_settings", Severity: SeverityRecommended,
		Label: "Servisler ayarları okuyabiliyor mu",
	}
	targets := map[string]string{"collector": roles.Collector, "beacon": roles.Beacon}

	var without []string
	var fixes []string
	checked := 0
	for _, name := range []string{"beacon", "collector"} {
		role := targets[name]
		if role == "" {
			continue
		}
		// A role that does not exist means this deployment never split
		// its roles - the check does not apply rather than failing. Saying
		// "beacon_writer cannot read settings" about a role nobody created
		// would send an installer looking for a grant on nothing.
		exists, err := s.roleExists(ctx, role)
		if err != nil {
			result.Status, result.Detail = CheckSkip, "Roller sorgulanamadı: "+err.Error()
			return result
		}
		if !exists {
			continue
		}
		checked++
		ok, err := s.roleHasPrivilege(ctx, role, "panel_settings", "SELECT")
		if err != nil {
			result.Status, result.Detail = CheckSkip, "Yetkiler sorgulanamadı: "+err.Error()
			return result
		}
		if !ok {
			without = append(without, role)
			fixes = append(fixes, fmt.Sprintf("GRANT SELECT ON panel_settings TO %s;", role))
		}
	}
	if checked == 0 {
		result.Status = CheckSkip
		result.Detail = "Ayrı servis rolleri bulunamadı; bu kurulum rolleri ayırmamış olabilir."
		return result
	}
	if len(without) > 0 {
		result.Status = CheckWarn
		result.Detail = strings.Join(without, ", ") + " ayarları okuyamıyor. Kurulum çalışır, " +
			"ama panelden yapılan ayar değişiklikleri bu servislere ulaşmaz — her değişiklik SSH ister."
		result.Fix = strings.Join(fixes, "\n")
		return result
	}
	result.Status, result.Detail = CheckPass, "Servisler panel_settings tablosunu okuyabiliyor."
	return result
}

// checkPanelIsolation is a negative check, and one of the two most
// important lines in this list.
//
// The panel reads analytics through the read-only HTTP API precisely so
// that the component a customer logs into has no direct route to the
// traffic data. A deployment where somebody granted a little too much
// looks completely healthy until the day it matters.
func (s *Store) checkPanelIsolation(ctx context.Context, roles PreflightRoles) CheckResult {
	result := CheckResult{
		ID: "grants.panel_isolation", Severity: SeverityRequired,
		Label: "Panel rolü analitik tablolara erişemiyor",
	}
	if roles.Panel == "" {
		result.Status, result.Detail = CheckSkip, "Panel rol adı verilmedi."
		return result
	}

	var leaked []string
	for _, table := range []string{"traffic_snapshots", "beacon_events"} {
		for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
			ok, err := s.roleHasPrivilege(ctx, roles.Panel, table, privilege)
			if err != nil {
				result.Status, result.Detail = CheckSkip, "Yetkiler sorgulanamadı: "+err.Error()
				return result
			}
			if ok {
				leaked = append(leaked, fmt.Sprintf("%s ON %s", privilege, table))
			}
		}
	}
	if len(leaked) > 0 {
		result.Status = CheckFail
		result.Detail = roles.Panel + " rolünün olmaması gereken yetkileri var: " + strings.Join(leaked, ", ") +
			". Panel analitiği salt okunur HTTP API üzerinden okur; doğrudan erişim bu ayrımı ortadan kaldırır."
		result.Fix = fmt.Sprintf("REVOKE ALL ON traffic_snapshots, beacon_events FROM %s;", roles.Panel)
		return result
	}
	result.Status, result.Detail = CheckPass, "Panel rolü analitik tablolara erişemiyor."
	return result
}

// checkAPIIsReadOnly is the other negative check. The read API's
// read-only guarantee is a property of its database role, not of its
// code - and it is what makes a support token safe to hand out.
func (s *Store) checkAPIIsReadOnly(ctx context.Context, roles PreflightRoles) CheckResult {
	result := CheckResult{
		ID: "grants.api_read_only", Severity: SeverityRequired,
		Label: "Okuma API'si gerçekten yazamıyor",
	}
	if roles.API == "" {
		result.Status, result.Detail = CheckSkip, "API rol adı verilmedi."
		return result
	}

	var writable []string
	for _, table := range []string{"traffic_snapshots", "beacon_events", "panel_users", "panel_settings"} {
		for _, privilege := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
			ok, err := s.roleHasPrivilege(ctx, roles.API, table, privilege)
			if err != nil {
				result.Status, result.Detail = CheckSkip, "Yetkiler sorgulanamadı: "+err.Error()
				return result
			}
			if ok {
				writable = append(writable, fmt.Sprintf("%s ON %s", privilege, table))
			}
		}
	}
	if len(writable) > 0 {
		result.Status = CheckFail
		result.Detail = roles.API + " rolü yazabiliyor: " + strings.Join(writable, ", ") +
			". Destek erişim token'ının güvenliği bu rolün yazamamasına dayanıyor."
		result.Fix = fmt.Sprintf("REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA public FROM %s;", roles.API)
		return result
	}
	result.Status, result.Detail = CheckPass, "API rolü hiçbir tabloya yazamıyor."
	return result
}

func (s *Store) checkRetentionPolicies(ctx context.Context) CheckResult {
	result := CheckResult{
		ID: "retention.configured", Severity: SeverityRecommended,
		Label: "Saklama süresi politikaları kurulu mu",
		Fix:   "Panelden: Ayarlar → Analitik verisi saklama süresi",
	}
	var jobs int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM timescaledb_information.jobs
		WHERE proc_name = 'policy_retention'
		  AND hypertable_name IN ('traffic_snapshots', 'beacon_events')`).Scan(&jobs)
	if err != nil {
		result.Status, result.Detail = CheckSkip, "TimescaleDB iş listesi okunamadı: "+err.Error()
		return result
	}
	if jobs == 0 {
		result.Status = CheckWarn
		result.Detail = "Hiçbir tabloda saklama politikası yok. İki analitik tablosu da sınırsız büyür; " +
			"dolan disk collector'ı durdurur ve ilk belirti sitenin yavaşlaması olur."
		return result
	}
	result.Status, result.Detail = CheckPass, fmt.Sprintf("%d saklama politikası kurulu.", jobs)
	return result
}

// missingTables returns which of want do not exist.
func (s *Store) missingTables(ctx context.Context, want []string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = ANY($1)`, want)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var missing []string
	for _, name := range want {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

// roleHasPrivilege asks PostgreSQL directly rather than reading
// information_schema.
//
// has_table_privilege answers for any role, where information_schema
// shows only grants the current user is party to - so the panel could
// not otherwise see what the beacon's role may do. A role that does not
// exist is reported as "no privilege" rather than as an error: a
// deployment that never created a separate collector role is a
// deployment where the check simply does not apply.
func (s *Store) roleHasPrivilege(ctx context.Context, role, table, privilege string) (bool, error) {
	exists, err := s.roleExists(ctx, role)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	var has bool
	// Both arguments are bound parameters, not interpolated: the role
	// name reaches this from configuration, and configuration is input
	// like any other.
	if err := s.pool.QueryRow(ctx,
		`SELECT has_table_privilege($1, $2, $3)`, role, table, privilege).Scan(&has); err != nil {
		return false, err
	}
	return has, nil
}

// checkDeveloperPassword reports whether the settings with legal weight
// can be changed from the panel at all.
//
// Not required, and that is a considered choice rather than leniency.
// Without a developer password those settings stay at their defaults,
// and the defaults are the privacy-preserving values - masked addresses,
// ninety days, no raw click identifiers. A deployment that wants them
// frozen there has made a defensible decision, and a wizard that refused
// to finish would be pushing the installer to weaken it. What the check
// must not do is stay quiet: somebody will eventually try to change one
// of these and needs to know why they cannot.
func checkDeveloperPassword(gate *devgate.Gate) CheckResult {
	result := CheckResult{
		ID: "config.developer_password", Label: "Geliştirici şifresi (hukuki ağırlıklı ayarlar)",
		Severity: SeverityRecommended,
	}
	if gate == nil {
		result.Status, result.Detail = CheckSkip, "Geliştirici kapısı bu kontrole verilmedi."
		return result
	}

	guarded := make([]string, 0, len(GuardedKeys()))
	for _, key := range GuardedKeys() {
		guarded = append(guarded, string(key))
	}

	if !gate.Configured() {
		result.Status = CheckWarn
		result.Detail = fmt.Sprintf(
			"Tanımlı değil. Şu %d ayar varsayılanında donmuş durumda ve panelden değiştirilemez: %s",
			len(guarded), strings.Join(guarded, ", "))
		result.Fix = "go run ./cmd/devpass  →  çıktıyı yapılandırma dosyasındaki [developer] password_hash alanına yazın"
		return result
	}
	result.Status = CheckPass
	result.Detail = fmt.Sprintf("Tanımlı. %d ayar her değişiklikte şifre soruyor.", len(guarded))
	return result
}

// checkIPTokenKey reports whether full IP mode could be switched on at
// all.
//
// Recommended rather than required, and skipped rather than failed when
// absent - a deployment that never leaves masked mode needs no key and
// is not misconfigured for lacking one. What the check exists for is the
// other direction: somebody about to ask why the panel refuses to switch
// modes should find the answer here rather than in a support call.
// botDataStaleAfter is when a fetched fingerprint set starts being worth
// mentioning. Not an expiry: last month's fingerprints are far better
// than none, so this warns and never fails.
const botDataStaleAfter = 30 * 24 * time.Hour

// checkBotData reports the known-bot fingerprint set.
//
// This project ships no copy of that dataset - see internal/botdata for
// why - which makes "never fetched" an ordinary state and makes saying
// so the whole job of this check. A deployment that quietly has no
// known-bot signal is the failure mode; a deployment that knows it does
// not is fine.
//
// Recommended rather than required: the collector works without it, and
// blocking an installation over a third party's data would be the wrong
// trade.
func checkBotData(path string, now func() time.Time) CheckResult {
	result := CheckResult{
		ID: "data.bot_fingerprints", Severity: SeverityRecommended,
		Label: "Bilinen bot parmak izleri getirildi mi",
		Fix:   "collector -config <dosya> -update-bot-data   (cron'a bağlayın)",
	}
	if path == "" {
		result.Status = CheckSkip
		result.Detail = "Panele bildirilmemiş. Collector yapılandırmasındaki bot_data.path " +
			"burada da tanımlanmadan bu kontrol bakamaz."
		return result
	}
	set, err := botdata.Load(path)
	if err != nil {
		result.Status = CheckFail
		result.Detail = "Dosya var ama okunamıyor: " + err.Error()
		return result
	}
	if !set.Fetched() {
		result.Status = CheckWarn
		result.Detail = "Hiç getirilmedi. Bu proje bu veri kümesini dağıtmıyor — kurulum " +
			"kendi makinesine, kaynağın kendi şartlarıyla indirir. Getirilene kadar " +
			"bilinen-bot sinyali yok; diğer sinyaller çalışmaya devam eder."
		return result
	}
	if now == nil {
		now = time.Now
	}
	age := now().Sub(set.FetchedAt)
	if age > botDataStaleAfter {
		result.Status = CheckWarn
		result.Detail = fmt.Sprintf("%d parmak izi var ama %d gün önce getirilmiş. "+
			"Eski liste yoktan iyidir; yine de tazelenmesi gerekir.",
			set.Len(), int(age.Hours()/24))
		return result
	}
	result.Status = CheckPass
	result.Detail = fmt.Sprintf("%d parmak izi, %d gün önce getirildi (kaynak: %s).",
		set.Len(), int(age.Hours()/24), set.Source)
	return result
}

func (s *Store) checkIPTokenKey() CheckResult {
	result := CheckResult{
		ID: "config.ip_token_key", Label: "IP jeton anahtarı (yalnız full mod için)",
		Severity: SeverityRecommended,
	}
	if s.IPTokenKeyConfigured() {
		result.Status = CheckPass
		result.Detail = "Tanımlı. IP saklama biçimi full'e alınabilir; ham adres yine saklanmaz."
		return result
	}
	result.Status = CheckSkip
	result.Detail = "Tanımlı değil. Kurulum maskeli modda çalışır ve bu sorun değildir — " +
		"anahtar yalnızca full moda geçilmek istenirse gerekir."
	result.Fix = "go run ./cmd/devpass -ipkey  →  aynı değeri hem collector hem beacon " +
		"yapılandırmasındaki [privacy] ip_hash_key alanına yazın"
	return result
}

// checkConfiguredRolesExist catches a typo in a role name.
//
// Without it, a misspelled role makes the two isolation checks silently
// inapplicable: has_table_privilege reports no privileges for a role
// nobody created, so "cannot read analytics" passes for a role that does
// not exist. The isolation would look verified when nothing was
// verified, which is the one outcome worse than an unverified check.
func (s *Store) checkConfiguredRolesExist(ctx context.Context, roles PreflightRoles) CheckResult {
	result := CheckResult{
		ID: "grants.roles_exist", Severity: SeverityRecommended,
		Label: "Yapılandırılan roller veritabanında var mı",
	}
	configured := map[string]string{
		"collector": roles.Collector, "beacon": roles.Beacon,
		"api": roles.API, "panel": roles.Panel,
	}

	var missing []string
	var named int
	for _, which := range []string{"collector", "beacon", "api", "panel"} {
		role := configured[which]
		if role == "" {
			continue
		}
		named++
		exists, err := s.roleExists(ctx, role)
		if err != nil {
			result.Status, result.Detail = CheckSkip, "Roller sorgulanamadı: "+err.Error()
			return result
		}
		if !exists {
			missing = append(missing, fmt.Sprintf("%s (%s)", role, which))
		}
	}
	if named == 0 {
		result.Status, result.Detail = CheckSkip, "Rol adı verilmedi."
		return result
	}
	if len(missing) > 0 {
		result.Status = CheckWarn
		result.Detail = "Yapılandırmada adı geçip veritabanında bulunmayan rol: " + strings.Join(missing, ", ") +
			". Yazım hatasıysa, o role bakan yetki kontrolleri hiçbir şeyi doğrulamadan geçer."
		result.Fix = `psql "$DSN" -c "\du"  # gerçek rol adlarını listeler`
		return result
	}
	result.Status, result.Detail = CheckPass, fmt.Sprintf("Adı geçen %d rolün hepsi mevcut.", named)
	return result
}

// roleExists reports whether a database role is defined.
func (s *Store) roleExists(ctx context.Context, role string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&exists)
	return exists, err
}

// --- filesystem and service checks ---

func checkLogDir(dir string) CheckResult {
	result := CheckResult{
		ID: "logs.writable", Severity: SeverityRequired,
		Label: "Günlük kaydı dizini yazılabilir mi",
	}
	if dir == "" {
		result.Status = CheckSkip
		result.Detail = "Günlük dizini yapılandırılmamış; kayıtlar yalnızca stderr'e gidiyor."
		result.Fix = `beacon.toml → [logging] dir = "/var/log/crucible"`
		return result
	}

	info, err := os.Stat(dir)
	if err != nil {
		result.Status, result.Detail = CheckFail, dir+" okunamadı: "+err.Error()
		result.Fix = fmt.Sprintf("mkdir -p %s && chown crucible: %s && chmod 700 %s", dir, dir, dir)
		return result
	}
	if !info.IsDir() {
		result.Status, result.Detail = CheckFail, dir+" bir dizin değil."
		return result
	}

	// Actually write, rather than reading the mode and inferring. The
	// mode can look right while the filesystem is mounted read-only or
	// the process runs as another user.
	probe := filepath.Join(dir, ".crucible-preflight")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		result.Status, result.Detail = CheckFail, dir+" yazılabilir değil: "+err.Error()
		result.Fix = fmt.Sprintf("chown -R crucible: %s && chmod 700 %s", dir, dir)
		return result
	}
	_ = os.Remove(probe)

	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		result.Status = CheckWarn
		result.Detail = fmt.Sprintf("%s izinleri %o. Log satırları IP ve tarayıcı bilgisi taşır, yani kişisel veridir.", dir, perm)
		result.Fix = fmt.Sprintf("chmod 700 %s", dir)
		return result
	}
	result.Status, result.Detail = CheckPass, dir+" yazılabilir ve yalnızca sahibine açık."
	return result
}

// checkFreeSpace measures the volumes the deployment writes to.
//
// Both are checked, not one: the logs and the database are routinely on
// different volumes, and a check that looked only at the data directory
// would report plenty of room while the log volume filled - which is the
// one that fills first.
func checkFreeSpace(paths map[string]string, minFree uint64) CheckResult {
	result := CheckResult{
		ID: "disk.free", Severity: SeverityRecommended,
		Label: "Disk boş alanı",
	}
	if len(paths) == 0 {
		result.Status, result.Detail = CheckSkip, "Ölçülecek dizin verilmedi."
		return result
	}

	var reports []string
	var low []string
	var measured int
	for _, name := range sortedKeys(paths) {
		free, err := freeBytes(paths[name])
		if err != nil {
			reports = append(reports, fmt.Sprintf("%s: ölçülemedi (%v)", name, err))
			continue
		}
		measured++
		reports = append(reports, fmt.Sprintf("%s: %s boş", name, humanBytes(free)))
		if free < minFree {
			low = append(low, name)
		}
	}
	if measured == 0 {
		result.Status, result.Detail = CheckSkip, strings.Join(reports, "; ")
		return result
	}
	if len(low) > 0 {
		result.Status = CheckFail
		result.Detail = strings.Join(reports, "; ") +
			fmt.Sprintf(". %s birimi %s altında; dolan disk collector'ı durdurur.",
				strings.Join(low, ", "), humanBytes(minFree))
		result.Fix = "Saklama sürelerini kısaltın veya diski büyütün."
		return result
	}
	result.Status, result.Detail = CheckPass, strings.Join(reports, "; ")
	return result
}

func checkService(ctx context.Context, client *http.Client, name, url string) CheckResult {
	result := CheckResult{
		ID: "service." + name, Severity: SeverityRequired,
		Label: name + " servisi çalışıyor mu",
		Fix:   fmt.Sprintf("systemctl status crucible-%s && systemctl start crucible-%s", name, name),
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		result.Status, result.Detail = CheckSkip, "Geçersiz adres: "+err.Error()
		return result
	}
	resp, err := client.Do(req)
	if err != nil {
		result.Status, result.Detail = CheckFail, url+" adresine ulaşılamadı: "+err.Error()
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		result.Status, result.Detail = CheckFail, fmt.Sprintf("%s HTTP %d döndü.", url, resp.StatusCode)
		return result
	}
	result.Status, result.Detail = CheckPass, url+" yanıt veriyor."
	return result
}

// checkBackups is honest rather than useful, and says so.
//
// Nothing in this system takes backups yet (see PLAN.md group F), so
// there is no state to inspect and no way to distinguish "backed up
// elsewhere" from "not backed up at all". Reporting a pass would be a
// lie, and reporting a failure would block handover on something the
// installer may well have handled with their own tooling. A warning that
// states the position exactly is the only honest option.
func checkBackups() CheckResult {
	return CheckResult{
		ID: "backup.configured", Severity: SeverityRecommended, Status: CheckWarn,
		Label:  "Yedekleme",
		Detail: "Bu sistem yedek almaz ve alınıp alınmadığını kontrol edemez. Müşterinin analitik geçmişi şu anda tek diske emanet.",
		Fix:    `Zamanlanmış pg_dump kurun, örn: 0 3 * * * pg_dump "$DSN" | gzip > /yedek/analytics-$(date +\%F).sql.gz`,
	}
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ManualStep is something the panel can never do, listed at the end of
// the wizard so the installer knows what is still theirs.
//
// Separate from CheckResult because these are not pass or fail - they
// are the boundary of what a web page can reach. A panel cannot renew a
// TLS certificate, cannot write a systemd unit, and cannot restore a
// backup, and pretending otherwise in a checklist would be the same
// dishonesty as a progress bar that fills on a timer.
type ManualStep struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Why says what makes it impossible from the panel, in one sentence.
	// Without it the list reads as an arbitrary set of chores.
	Why string `json:"why"`
	// Command is what to run on the server, where there is one.
	Command string `json:"command,omitempty"`
	// CheckedBy names the automatic check that verifies this step, when
	// one exists. A step with no checker has to be confirmed by hand,
	// and the wizard says which is which rather than blurring them.
	CheckedBy string `json:"checked_by,omitempty"`
}

// ManualSteps is the list shown at the end of the setup wizard.
//
// Ordered as the work is actually done, not by importance: an installer
// reads this standing at a terminal, and a list they can work straight
// down is worth more than one sorted by severity.
func ManualSteps() []ManualStep {
	return []ManualStep{
		{
			ID: "db.install", Label: "PostgreSQL ve TimescaleDB kurulumu",
			Why:     "Panel, üzerinde çalıştığı veritabanını kuramaz.",
			Command: "apt install postgresql timescaledb-2-postgresql-16",
		},
		{
			ID: "db.roles", Label: "Dört veritabanı rolü ve yetkileri",
			Why: "Rol ayrımı bu sistemin güvenlik temelinin yarısı, ve bir rol kendine " +
				"yetki veremez - verebilseydi ayrımın bir anlamı kalmazdı.",
			Command:   "README'deki CREATE ROLE / GRANT bloklarını uygulayın",
			CheckedBy: "grants.panel_isolation, grants.api_read_only, grants.live_settings",
		},
		{
			ID: "db.schema", Label: "Şema dosyalarının uygulanması",
			Why: "Hiçbir servis DDL çalıştırmaz. Bu bilinçli: şema değişikliği, " +
				"çalışan bir sürecin kendi kendine yapabileceği bir şey olmamalı.",
			Command:   `for f in internal/*/schema.sql; do psql "$DSN" -f "$f"; done`,
			CheckedBy: "schema.panel, schema.analytics, schema.columns",
		},
		{
			ID: "data.bot_fingerprints", Label: "Bilinen bot parmak izlerinin getirilmesi",
			Why: "Bu veri kümesi bize ait değil ve bu proje onu dağıtmıyor - kurulum kendi " +
				"makinesine, kaynağın kendi şartlarıyla indirir. Ne zaman ve nasıl " +
				"tazeleneceği de kurulumun kararı; panel bir başkasının sunucusuna sizin " +
				"adınıza istek atmaz.",
			Command:   "collector -config <dosya> -update-bot-data   (cron'a bağlayın)",
			CheckedBy: "data.bot_fingerprints",
		},
		{
			ID: "config.bootstrap", Label: "Yapılandırma dosyasındaki sekiz anahtar",
			Why: "Veritabanına nasıl ulaşılacağını veritabanına soramazsınız. DSN, dinleme " +
				"adresleri, TLS yolları ve site kimliği bu yüzden dosyada kalır.",
			Command: "crucible.toml: timescale_dsn, listen_addr, backend_addr, mode, tls.*, site_id",
		},
		{
			ID: "config.developer_password", Label: "Geliştirici şifresi hash'i",
			Why: "Hukuki ağırlığı olan ayarlar (IP saklama biçimi, saklama süreleri, " +
				"kampanya parametreleri) her değişiklikte ayrı bir şifre ister. O şifre " +
				"veritabanında değil yapılandırma dosyasında durur - kasıtlı olarak: " +
				"paneli ele geçiren bir şeyin erişemeyeceği tek yer orası. Düz metin " +
				"değil, hash yazılır.",
			Command:   "go run ./cmd/devpass  →  [developer] password_hash = \"$argon2id$...\"",
			CheckedBy: "config.developer_password",
		},
		{
			ID: "config.ip_token_key", Label: "IP jeton anahtarı",
			Why: "Hiçbir modda ham IP adresi saklanmaz. Maskeli modda yalnız ağ " +
				"(IPv4 /24) yazılır ve anahtar gerekmez. full modda aynı maskeli ağın " +
				"yanına tam adresten türetilmiş anahtarlı bir jeton yazılır — aynı /24 " +
				"içindeki iki ziyaretçi ayrılabilir, adres yine diske inmez. Bu anahtar " +
				"iki serviste de birebir aynı olmalıdır; farklı olursa kesişim birleşimi " +
				"hatasız biçimde boş döner.",
			Command:   "go run ./cmd/devpass -ipkey  →  [privacy] ip_hash_key (collector + beacon)",
			CheckedBy: "config.ip_token_key",
		},
		{
			ID: "logs.dir", Label: "Günlük dizini ve izinleri",
			Why:       "Panel, kendi yazacağı dizini oluşturamaz - henüz çalışmıyor olabilir.",
			Command:   "mkdir -p /var/log/crucible && chown crucible: /var/log/crucible && chmod 700 /var/log/crucible",
			CheckedBy: "logs.writable",
		},
		{
			ID: "systemd", Label: "systemd unit dosyaları",
			Why: "Panelin süreç başlatma yetkisi yoktur ve olmamalıdır - yalnızca temiz " +
				"çıkabilir, systemd yeniden başlatır. Başlatabilen bir onarım yüzeyi, " +
				"rastgele bir şey başlatan bir yüzeye çevrilebilirdi.",
			Command:   "deploy/systemd/*.service dosyalarını /etc/systemd/system/ altına kopyalayın",
			CheckedBy: "service.collector, service.beacon, service.api",
		},
		{
			ID: "tls", Label: "TLS sertifikası ve yenilemesi",
			Why:     "Sertifika dosya sistemindedir ve yenilemesi kök yetkisi ister.",
			Command: "certbot certonly --standalone -d example.com",
		},
		{
			ID: "proxy", Label: "Web sunucusunda /_ca/ yönlendirmesi",
			Why: "Beacon'ın sitenin kendi kaynağından sunulması gerekir; bunu yapan " +
				"yapılandırma sitenin web sunucusunda, bizde değil.",
			Command: "nginx: location /_ca/ { proxy_pass http://127.0.0.1:8081; }",
		},
		{
			ID: "backup", Label: "Yedekleme",
			Why: "Bu sistem yedek almaz ve alınıp alınmadığını göremez. Müşterinin analitik " +
				"geçmişi aksi hâlde tek diske emanettir.",
			Command:   `0 3 * * * pg_dump "$DSN" | gzip > /yedek/analytics-$(date +\%F).sql.gz`,
			CheckedBy: "",
		},
		{
			ID: "disk", Label: "Disk alanı planlaması",
			Why:       "Dolan disk collector'ı durdurur, yani analitik özelliği trafik yolunu düşürür.",
			CheckedBy: "disk.free",
		},
	}
}

// UncheckedSteps returns the manual steps no automatic check covers.
//
// These are the ones the installer has to confirm by hand, and the
// wizard must show them as exactly that. A checklist that presents a
// verified item and an unverified one identically teaches the reader to
// trust neither.
func UncheckedSteps() []ManualStep {
	var out []ManualStep
	for _, step := range ManualSteps() {
		if step.CheckedBy == "" {
			out = append(out, step)
		}
	}
	return out
}

// errNoPath is what freeBytes returns for an empty path, so both
// implementations agree on that case.
var errNoPath = errors.New("dizin verilmedi")
