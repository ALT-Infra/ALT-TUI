package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"altv1/internal/profile"
)

var ErrProfileRevisionConflict = errors.New("profile revision already exists with different content")
var ErrProfileDraftStale = errors.New("team draft is based on a stale revision")

type ProfileSummary struct {
	ID        string
	Revision  int
	Digest    string
	Name      string
	CreatedAt time.Time
}

func (s *Store) ImportProfile(ctx context.Context, document *profile.Document) error {
	diagnostics := profile.Validate(document.Profile)
	if profile.HasErrors(diagnostics) {
		return fmt.Errorf("team profile is invalid")
	}

	var existing string
	err := s.db.QueryRowContext(ctx,
		`SELECT digest FROM profile_revisions WHERE profile_id = ? AND revision = ?`,
		document.Profile.ID, document.Profile.Revision,
	).Scan(&existing)
	if err == nil {
		if existing == document.Digest {
			return nil
		}
		return ErrProfileRevisionConflict
	}
	if !isNotFound(err) {
		return fmt.Errorf("query profile revision: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO profile_revisions(profile_id, revision, digest, name, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		document.Profile.ID,
		document.Profile.Revision,
		document.Digest,
		document.Profile.Name,
		document.Source,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("import profile revision: %w", err)
	}
	return nil
}

// PublishProfile turns a mutable GUI draft into the next immutable revision.
// expectedBase is zero for a new team and the revision the editor loaded for
// an edit. The comparison and insert share one transaction, so two editor
// windows cannot accidentally publish the same revision.
func (s *Store) PublishProfile(
	ctx context.Context,
	value profile.Profile,
	expectedBase int,
) (*profile.Document, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin profile publish: %w", err)
	}
	defer tx.Rollback()

	var latest int
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(revision), 0) FROM profile_revisions WHERE profile_id = ?`,
		value.ID,
	).Scan(&latest)
	if err != nil {
		return nil, fmt.Errorf("query latest profile revision: %w", err)
	}
	if latest != expectedBase {
		return nil, fmt.Errorf("%w: loaded %d, latest is %d", ErrProfileDraftStale, expectedBase, latest)
	}
	value.Revision = latest + 1
	document, err := profile.FromValue(value)
	if err != nil {
		return nil, err
	}
	if diagnostics := profile.Validate(document.Profile); profile.HasErrors(diagnostics) {
		return nil, fmt.Errorf("team profile is invalid")
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO profile_revisions(profile_id, revision, digest, name, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		document.Profile.ID,
		document.Profile.Revision,
		document.Digest,
		document.Profile.Name,
		document.Source,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		if isSQLiteConstraint(err) {
			return nil, ErrProfileRevisionConflict
		}
		return nil, fmt.Errorf("publish profile revision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit profile publish: %w", err)
	}
	return document, nil
}

// modernc SQLite exposes the stable SQLite numeric result code through this
// interface. Masking the extended result code yields SQLITE_CONSTRAINT (19);
// no driver-authored English error text is interpreted.
func isSQLiteConstraint(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code()&0xff == 19
}

func (s *Store) Profile(ctx context.Context, id string, revision int) (*profile.Document, error) {
	query := `SELECT source FROM profile_revisions WHERE profile_id = ?`
	args := []any{id}
	if revision > 0 {
		query += ` AND revision = ?`
		args = append(args, revision)
	} else {
		query += ` ORDER BY revision DESC LIMIT 1`
	}
	var source []byte
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&source); err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("team profile %s revision %d not found", id, revision)
		}
		return nil, fmt.Errorf("load team profile: %w", err)
	}
	document, err := profile.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("stored team profile is invalid: %w", err)
	}
	return document, nil
}

func (s *Store) ListProfiles(ctx context.Context) ([]ProfileSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT profile_id, revision, digest, name, created_at
		FROM profile_revisions
		ORDER BY profile_id, revision DESC`)
	if err != nil {
		return nil, fmt.Errorf("list team profiles: %w", err)
	}
	defer rows.Close()

	var result []ProfileSummary
	for rows.Next() {
		var item ProfileSummary
		var created string
		if err := rows.Scan(&item.ID, &item.Revision, &item.Digest, &item.Name, &created); err != nil {
			return nil, fmt.Errorf("scan team profile: %w", err)
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		result = append(result, item)
	}
	return result, rows.Err()
}

func scanNullableString(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}
