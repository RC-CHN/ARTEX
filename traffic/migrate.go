package traffic

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
)

// migrateSQLite runs before the proxy starts. The old process must be stopped:
// source files are copied (including committed WAL pages), never opened in place.
// Import + verification + completion marker commit together. Failure leaves the
// old index untouched and PostgreSQL empty, so restarting safely retries it.
func (t *Traffic) migrateSQLite() error {
	var complete bool
	if err := t.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM migrations WHERE name='sqlite-v1')`).Scan(&complete); err != nil {
		return err
	}
	if complete {
		return nil
	}
	source := filepath.Join(t.dir, "_index", "index.sqlite")
	if _, err := os.Stat(source); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if st, err := os.Stat(source + "-journal"); err == nil && st.Size() > 0 {
		return errors.New("legacy SQLite has a rollback journal; cleanly stop/recover the old instance before migration")
	}
	tx, err := t.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(7337741004)`); err != nil {
		return err
	}
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM migrations WHERE name='sqlite-v1')`).Scan(&complete); err != nil {
		return err
	}
	if complete {
		return nil
	}
	var existing int64
	if err := tx.QueryRow(`SELECT count(*) FROM exchanges`).Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		return errors.New("refusing to merge legacy SQLite into a non-empty PostgreSQL traffic index")
	}

	scratch, err := os.MkdirTemp("", "artex-traffic-migration-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	copyPath := filepath.Join(scratch, "index.sqlite")
	fingerprint, err := snapshotSQLite(source, copyPath)
	if err != nil {
		return err
	}
	u := &url.URL{Scheme: "file", Path: copyPath}
	legacy, err := sql.Open("sqlite", u.String()+"?mode=ro")
	if err != nil {
		return err
	}
	defer legacy.Close()
	legacy.SetMaxOpenConns(1)
	var integrity string
	if err := legacy.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("legacy SQLite integrity check: %s", integrity)
	}
	// Older layouts may have only exchanges; retain their file paths and original
	// body files so Get/ReadEvidence continue to serve historical captures.
	var hasBodies, hasRefs bool
	if err := legacy.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='exchange_bodies')`).Scan(&hasBodies); err != nil {
		return err
	}
	if err := legacy.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='blob_refs')`).Scan(&hasRefs); err != nil {
		return err
	}
	var sourceExchanges, sourceBodies int64
	if err := legacy.QueryRow(`SELECT count(*) FROM exchanges`).Scan(&sourceExchanges); err != nil {
		return err
	}
	if hasBodies {
		if err := legacy.QueryRow(`SELECT count(*) FROM exchange_bodies`).Scan(&sourceBodies); err != nil {
			return err
		}
	}
	verifiedBlobs := map[string]bool{}
	verifyBlob := func(hash string) error {
		if hash == "" || verifiedBlobs[hash] {
			return nil
		}
		if err := t.verifyMigrationBlob(hash); err != nil {
			return err
		}
		verifiedBlobs[hash] = true
		return nil
	}
	query := `SELECT e.id,e.ts,e.host,e.method,e.url_template,e.url,e.status,e.content_type,e.req_len,e.resp_len,e.path,`
	if hasBodies {
		query += `b.id IS NOT NULL,COALESCE(b.req_head,''),b.req_body,COALESCE(b.req_blob,''),COALESCE(b.resp_head,''),b.resp_body,COALESCE(b.resp_blob,'') FROM exchanges e LEFT JOIN exchange_bodies b ON b.id=e.id ORDER BY e.id`
	} else {
		query += `false,'',NULL,'','',NULL,'' FROM exchanges e ORDER BY e.id`
	}
	rows, err := legacy.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	var exchanges, bodies int64
	for rows.Next() {
		var item ArchiveExchange
		var path string
		var hasBody bool
		if err := rows.Scan(&item.ID, &item.TS, &item.Host, &item.Method, &item.URLTemplate, &item.URL,
			&item.Status, &item.ContentType, &item.ReqLen, &item.RespLen, &path, &hasBody,
			&item.ReqHead, &item.ReqBody, &item.ReqBlob, &item.RespHead, &item.RespBody, &item.RespBlob); err != nil {
			return err
		}
		for _, hash := range []string{item.ReqBlob, item.RespBlob} {
			if err := verifyBlob(hash); err != nil {
				return err
			}
		}
		searchItem := item
		if !hasBody && path != "" {
			if !filepath.IsLocal(path) {
				return fmt.Errorf("invalid legacy path for exchange %s", item.ID)
			}
			for _, side := range []struct {
				name string
				head *string
				body *[]byte
			}{
				{"request.http", &searchItem.ReqHead, &searchItem.ReqBody},
				{"response.http", &searchItem.RespHead, &searchItem.RespBody},
			} {
				f, err := os.Open(filepath.Join(t.dir, path, side.name))
				if err != nil {
					return fmt.Errorf("legacy exchange %s: %w", item.ID, err)
				}
				head, body, readErr := splitLegacyEvidence(f)
				if readErr == nil {
					*side.head = head
					*side.body, readErr = io.ReadAll(io.LimitReader(body, maxIndexBody))
				}
				f.Close()
				if readErr != nil {
					return readErr
				}
			}
		}
		content, err := t.exchangeSearchText(searchItem)
		if err != nil {
			return fmt.Errorf("rebuild search for exchange %s: %w", item.ID, err)
		}
		args := exchangeArgs(item, path, content)
		args[18] = hasBody
		if _, err := tx.Exec(postgresInsertExchange, args...); err != nil {
			return fmt.Errorf("import exchange %s: %w", item.ID, err)
		}
		exchanges++
		if hasBody {
			bodies++
		}
		if exchanges%10000 == 0 {
			log.Printf("[traffic] PostgreSQL migration: %d exchanges copied", exchanges)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	// Copy all source references, including historical references not represented
	// in inline body columns. Re-checking references protects later blob GC.
	if hasRefs {
		refs, err := legacy.Query(`SELECT hash,exchange_id FROM blob_refs ORDER BY hash,exchange_id`)
		if err != nil {
			return err
		}
		for refs.Next() {
			var hash, id string
			if err := refs.Scan(&hash, &id); err != nil {
				refs.Close()
				return err
			}
			if err := verifyBlob(hash); err != nil {
				refs.Close()
				return err
			}
			if _, err := tx.Exec(`INSERT INTO blob_refs(hash,exchange_id) VALUES(?,?) ON CONFLICT DO NOTHING`, hash, id); err != nil {
				refs.Close()
				return err
			}
		}
		err = refs.Err()
		refs.Close()
		if err != nil {
			return err
		}
	}
	var gotExchanges, gotBodies, gotSearch, gotRefs int64
	if err := tx.QueryRow(`SELECT (SELECT count(*) FROM exchanges),(SELECT count(*) FROM exchange_bodies),(SELECT count(*) FROM ex_fts),(SELECT count(*) FROM blob_refs)`).Scan(&gotExchanges, &gotBodies, &gotSearch, &gotRefs); err != nil {
		return err
	}
	if gotExchanges != exchanges || gotBodies != bodies || gotSearch != exchanges || exchanges != sourceExchanges || bodies != sourceBodies {
		return fmt.Errorf("traffic migration count mismatch: exchanges=%d/%d bodies=%d/%d search=%d", gotExchanges, exchanges, gotBodies, bodies, gotSearch)
	}
	if _, err := tx.Exec(`INSERT INTO migrations(name,source_sha256,exchanges,bodies,blob_refs) VALUES('sqlite-v1',?,?,?,?)`, fingerprint, exchanges, bodies, gotRefs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("[traffic] PostgreSQL migration complete: %d exchanges, %d bodies, %d blob references; legacy SQLite retained for backup", exchanges, bodies, gotRefs)
	return nil
}

func (t *Traffic) verifyMigrationBlob(hash string) error {
	f, _, err := t.Blob(hash)
	if err != nil {
		return err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return err
	}
	if hex.EncodeToString(sum.Sum(nil)) != hash {
		return fmt.Errorf("legacy blob checksum mismatch: %s", hash)
	}
	return nil
}

// snapshotSQLite copies stable source pages plus WAL to ephemeral scratch space.
// No WAL/shared-memory operations run against the shared legacy source files.
func snapshotSQLite(source, destination string) (string, error) {
	sum := sha256.New()
	for _, suffix := range []string{"", "-wal"} {
		in, err := os.Open(source + suffix)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			return "", err
		}
		out, err := os.OpenFile(destination+suffix, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			in.Close()
			return "", err
		}
		_, copyErr := io.Copy(io.MultiWriter(out, sum), in)
		closeErr := out.Close()
		in.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return "", err
		}
	}
	check := sha256.New()
	for _, suffix := range []string{"", "-wal"} {
		in, err := os.Open(source + suffix)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = io.Copy(check, in)
		in.Close()
		if err != nil {
			return "", err
		}
	}
	fingerprint := hex.EncodeToString(sum.Sum(nil))
	if fingerprint != hex.EncodeToString(check.Sum(nil)) {
		return "", errors.New("legacy SQLite changed during snapshot; stop the old instance before migration")
	}
	return fingerprint, nil
}
