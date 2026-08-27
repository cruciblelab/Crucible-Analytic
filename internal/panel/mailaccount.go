package panel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cruciblelab/crucible-analytic/internal/mail"
	"github.com/cruciblelab/crucible-analytic/internal/sealed"
)

// The outgoing mail account: one per deployment, stored in panel_smtp.
//
// # Why the password cannot leave here in the clear by accident
//
// There are two reads and they return different things. MailAccount is
// what every page uses and it has no password field at all - not an
// empty one, not a masked one, none. MailConfig is the only way to the
// plaintext and it exists to be handed to internal/mail.
//
// That split is the design. A single struct carrying the password would
// work perfectly and would put the live SMTP password one careless
// template range away from a browser, and template mistakes of that
// shape are not hypothetical - they are what "{{ . }}" does when
// somebody is debugging a form at midnight. A field that does not exist
// cannot be rendered.
//
// # Why the store seals rather than the caller
//
// SaveMailAccount takes the password as typed and encrypts it here. The
// alternative - a caller that seals and passes ciphertext - reads as
// more flexible and gives every future caller the chance to forget,
// once, silently, leaving a plaintext password in a column that everyone
// afterwards assumes is encrypted. There is no exported path in this
// package that writes password_sealed directly.

// MailAccount is the outgoing mail account as a page may see it.
type MailAccount struct {
	Configured bool
	Host       string
	Port       int
	Encryption mail.Encryption
	Username   string
	// HasPassword reports whether a password is stored, without being
	// one. This is the only thing any page needs to know about it: the
	// field says "leave blank to keep the current password" and that
	// sentence needs a boolean, not a secret.
	HasPassword bool
	// PasswordUnreadable is true when a password is stored and the
	// configured key cannot open it - the key was changed or replaced.
	//
	// Its own flag rather than an error, because the page still renders
	// and everything else on it is still true. What it must not do is
	// say "a password is saved" and then fail to send with something
	// that reads like a wrong password.
	PasswordUnreadable bool

	FromAddress string
	FromName    string
	Enabled     bool

	VerifiedAt         time.Time
	VerifiedOK         bool
	VerifiedDiagnosis  mail.Diagnosis
	VerifiedServerSaid string

	UpdatedAt time.Time
}

// ErrNoMailAccount means none has been configured.
var ErrNoMailAccount = errors.New("panel: no outgoing mail account is configured")

// mailPasswordLabel binds the sealed password to this column. Changing
// it makes every stored password unreadable, so it is a constant and not
// a parameter.
const mailPasswordLabel = "panel_smtp.password"

// MailAccount reads the account without its password.
func (s *Store) MailAccount(ctx context.Context, key sealed.Key) (MailAccount, error) {
	var (
		acc            MailAccount
		sealedPassword string
		verifiedAt     *time.Time
		encryption     string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT host, port, encryption, username, password_sealed,
		       from_address, from_name, enabled,
		       verified_at, verified_ok, verified_diagnosis, verified_server_said,
		       updated_at
		FROM panel_smtp WHERE id = 1`).Scan(
		&acc.Host, &acc.Port, &encryption, &acc.Username, &sealedPassword,
		&acc.FromAddress, &acc.FromName, &acc.Enabled,
		&verifiedAt, &acc.VerifiedOK, &acc.VerifiedDiagnosis, &acc.VerifiedServerSaid,
		&acc.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MailAccount{}, nil
	}
	if err != nil {
		return MailAccount{}, fmt.Errorf("panel: reading the mail account: %w", err)
	}

	acc.Configured = true
	acc.Encryption = mail.Encryption(encryption)
	if verifiedAt != nil {
		acc.VerifiedAt = *verifiedAt
	}

	if sealedPassword != "" {
		acc.HasPassword = true
		// Opened and thrown away. The question this answers is whether
		// the page should warn, and the only way to answer it honestly
		// is to try - a stored value and a readable stored value are
		// different facts, and the difference is invisible until a send
		// fails for a reason that looks like a wrong password.
		if _, openErr := key.Open(mailPasswordLabel, sealedPassword); openErr != nil {
			acc.PasswordUnreadable = true
		}
	}
	return acc, nil
}

// MailConfig reads the account as something that can send.
//
// The only path to the plaintext password. Returns ErrNoMailAccount when
// nothing is configured and when the account is switched off, so a
// caller cannot send through a disabled account by forgetting to check.
func (s *Store) MailConfig(ctx context.Context, key sealed.Key) (mail.Config, error) {
	var (
		cfg            mail.Config
		encryption     string
		sealedPassword string
		enabled        bool
	)
	err := s.pool.QueryRow(ctx, `
		SELECT host, port, encryption, username, password_sealed,
		       from_address, from_name, enabled
		FROM panel_smtp WHERE id = 1`).Scan(
		&cfg.Host, &cfg.Port, &encryption, &cfg.Username, &sealedPassword,
		&cfg.From, &cfg.FromName, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return mail.Config{}, ErrNoMailAccount
	}
	if err != nil {
		return mail.Config{}, fmt.Errorf("panel: reading the mail account: %w", err)
	}
	if !enabled {
		return mail.Config{}, ErrNoMailAccount
	}
	cfg.Encryption = mail.Encryption(encryption)

	if sealedPassword != "" {
		plain, openErr := key.Open(mailPasswordLabel, sealedPassword)
		if openErr != nil {
			// Refused rather than sent with an empty password. Sending
			// blank would come back as "authentication failed", which is
			// the one answer guaranteed to send somebody to check a
			// password that was never wrong.
			return mail.Config{}, fmt.Errorf("panel: the stored mail password: %w", openErr)
		}
		cfg.Password = plain
	}
	return cfg, nil
}

// MailAccountInput is a submitted mail account.
type MailAccountInput struct {
	Host       string
	Port       int
	Encryption mail.Encryption
	Username   string
	// Password as typed. Empty means "keep whatever is stored", which is
	// what a form showing no password has to mean - the alternative is a
	// page that silently erases the password every time somebody edits
	// the sender name.
	Password string
	// ClearPassword empties it. Explicit rather than inferred from an
	// empty Password, because those are different intentions and one of
	// them is destructive.
	ClearPassword bool

	FromAddress string
	FromName    string
	Enabled     bool
}

// Validate checks a submitted account before anything is stored.
func (in MailAccountInput) Validate() error {
	if strings.TrimSpace(in.Host) == "" {
		return errors.New("panel: the mail server address is required")
	}
	if strings.ContainsAny(in.Host, " \t\r\n") {
		return errors.New("panel: the mail server address contains whitespace")
	}
	if in.Port < 1 || in.Port > 65535 {
		return fmt.Errorf("panel: port %d is out of range (1..65535)", in.Port)
	}
	switch in.Encryption {
	case mail.EncryptionSTARTTLS, mail.EncryptionImplicit:
	default:
		return fmt.Errorf("panel: unknown encryption %q", in.Encryption)
	}
	if strings.TrimSpace(in.FromAddress) == "" {
		return errors.New("panel: the sender address is required")
	}
	// Validated by the package that will send it, rather than by a
	// second rule here. A page that accepts an address the sender then
	// refuses tells the person nothing useful, twice.
	if _, err := mail.ParseAddress(in.FromAddress); err != nil {
		return fmt.Errorf("panel: the sender address: %w", err)
	}
	return nil
}

// SaveMailAccount stores the account, encrypting the password.
//
// A password is written only when one was typed, so editing the sender
// name does not require retyping it. ClearPassword is the way to remove
// one, and it has to be asked for.
func (s *Store) SaveMailAccount(ctx context.Context, key sealed.Key, in MailAccountInput, by int64) error {
	if err := in.Validate(); err != nil {
		return err
	}
	if !key.IsSet() && in.Password != "" {
		// Refused rather than stored in the clear. This is the whole
		// reason the key exists, and the one moment where "just this
		// once" would produce a plaintext password in a column nobody
		// ever looks at again.
		return fmt.Errorf("panel: a mail password cannot be saved: %w", sealed.ErrNoKey)
	}

	var updatedBy *int64
	if by > 0 {
		updatedBy = &by
	}

	// Keep, replace, or clear - decided here and carried into the
	// statement as a value rather than as SQL.
	//
	// The first version built three fragments in Go and concatenated the
	// chosen one into the query. Nothing user-supplied reached it, so it
	// was not injectable; it was still string concatenation into SQL in a
	// package that goes to the trouble of a closed type elsewhere for
	// exactly this, and the fourth branch somebody adds in a hurry is the
	// one that interpolates something. A CASE expression says the same
	// three things in SQL, where a reader can see them next to the column
	// they write.
	var sealedPassword string
	if in.Password != "" && !in.ClearPassword {
		var err error
		sealedPassword, err = key.Seal(mailPasswordLabel, in.Password)
		if err != nil {
			return fmt.Errorf("panel: encrypting the mail password: %w", err)
		}
	}

	// Saving changes the account, so whatever the last verification said
	// is now about a different account. Cleared rather than kept: a page
	// reporting "verified in March" about a server address typed this
	// morning is worse than one reporting nothing.
	// One statement, no fragments. Every branch is in the SQL below and
	// every value is a parameter.
	const query = `
		INSERT INTO panel_smtp
		    (id, host, port, encryption, username, password_sealed,
		     from_address, from_name, enabled, updated_at, updated_by)
		VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, now(), $9)
		ON CONFLICT (id) DO UPDATE SET
		    host = EXCLUDED.host,
		    port = EXCLUDED.port,
		    encryption = EXCLUDED.encryption,
		    username = EXCLUDED.username,
		    -- Clear when asked, replace when one was typed, keep
		    -- otherwise. $10 is the clear flag; $5 is the sealed
		    -- password, empty when none was typed.
		    password_sealed = CASE
		        WHEN $10 THEN ''
		        WHEN $5 <> '' THEN $5
		        ELSE panel_smtp.password_sealed
		    END,
		    from_address = EXCLUDED.from_address,
		    from_name = EXCLUDED.from_name,
		    enabled = EXCLUDED.enabled,
		    verified_at = NULL,
		    verified_ok = FALSE,
		    verified_diagnosis = '',
		    verified_server_said = '',
		    updated_at = now(),
		    updated_by = EXCLUDED.updated_by`

	_, err := s.pool.Exec(ctx, query,
		strings.TrimSpace(in.Host), in.Port, string(in.Encryption),
		strings.TrimSpace(in.Username), sealedPassword,
		strings.TrimSpace(in.FromAddress), strings.TrimSpace(in.FromName),
		in.Enabled, updatedBy, in.ClearPassword)
	if err != nil {
		return fmt.Errorf("panel: saving the mail account: %w", err)
	}
	return nil
}

// RecordMailVerification stores the outcome of a connection test.
func (s *Store) RecordMailVerification(ctx context.Context, p mail.Probe) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE panel_smtp SET
		    verified_at = now(),
		    verified_ok = $1,
		    verified_diagnosis = $2,
		    verified_server_said = $3
		WHERE id = 1`,
		p.OK(), string(p.Diagnose()), mailServerSaid(p))
	if err != nil {
		return fmt.Errorf("panel: recording the mail verification: %w", err)
	}
	return nil
}

// DeleteMailAccount removes it entirely.
func (s *Store) DeleteMailAccount(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM panel_smtp WHERE id = 1`); err != nil {
		return fmt.Errorf("panel: deleting the mail account: %w", err)
	}
	return nil
}

// mailServerSaid picks the one line worth storing about a failure.
//
// The server's own reply when there was one, and the local detail
// otherwise. Never both concatenated: the panel shows this under a
// sentence that says where it came from, and a field holding two things
// makes that sentence a guess.
func mailServerSaid(p mail.Probe) string {
	if p.ServerSaid != "" {
		if p.ServerCode > 0 {
			return fmt.Sprintf("%d %s", p.ServerCode, p.ServerSaid)
		}
		return p.ServerSaid
	}
	return p.Detail
}
