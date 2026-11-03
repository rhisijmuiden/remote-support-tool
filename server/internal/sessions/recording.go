package sessions

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// MaxChunk is the largest chunk the viewer may upload in one request.
const MaxChunk = 16 << 20

// Store keeps encrypted recording chunks on disk under <data_dir>/recordings/<session id>/.
// Each chunk file is nonce || AES-256-GCM(chunk), with the session id and chunk number
// as additional data so chunks cannot be swapped between sessions or reordered.
type Store struct {
	Dir  string
	aead cipher.AEAD
}

// NewStore derives the recording key from the server secret_key. Recordings cannot be
// played back if secret_key changes, so keep it with your backups.
func NewStore(dataDir, secretKey string) (*Store, error) {
	k := sha256.Sum256([]byte("rhis-remote/recording/v1\x00" + secretKey))
	blk, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &Store{Dir: filepath.Join(dataDir, "recordings"), aead: a}, nil
}

func (st *Store) dir(id int64) string { return filepath.Join(st.Dir, strconv.FormatInt(id, 10)) }

func (st *Store) path(id int64, seq int) string {
	return filepath.Join(st.dir(id), fmt.Sprintf("%06d.chunk", seq))
}

func aad(id int64, seq int) []byte { return []byte(fmt.Sprintf("rhis-rec/v1|%d|%d", id, seq)) }

func (st *Store) write(id int64, seq int, data []byte) error {
	if err := os.MkdirAll(st.dir(id), 0o750); err != nil {
		return err
	}
	nonce := make([]byte, st.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	out := st.aead.Seal(nonce, nonce, data, aad(id, seq))
	tmp := st.path(id, seq) + ".tmp"
	if err := os.WriteFile(tmp, out, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, st.path(id, seq))
}

func (st *Store) read(id int64, seq int) ([]byte, error) {
	raw, err := os.ReadFile(st.path(id, seq))
	if err != nil {
		return nil, err
	}
	n := st.aead.NonceSize()
	if len(raw) < n {
		return nil, errors.New("recording chunk too short")
	}
	p, err := st.aead.Open(nil, raw[:n], raw[n:], aad(id, seq))
	if err != nil {
		return nil, errors.New("recording chunk cannot be decrypted (was secret_key changed?)")
	}
	return p, nil
}

// AppendChunk stores chunk seq (0, 1, 2, ...) of a session that is being recorded.
// Re-sending a chunk that was already stored is accepted and ignored, so the viewer can
// retry safely after a network error.
func (s *Service) AppendChunk(ctx context.Context, id int64, seq int, mime string, data []byte) error {
	if len(data) == 0 || len(data) > MaxChunk {
		return fmt.Errorf("chunk size %d out of range", len(data))
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var status string
	var chunks int
	if err := tx.QueryRow(ctx, `SELECT recording_status, recording_chunks FROM remote_sessions WHERE id=$1 FOR UPDATE`, id).Scan(&status, &chunks); err != nil {
		return ErrNotFound
	}
	if seq < chunks {
		return nil
	}
	if status != RecRecording {
		return ErrNotRecording
	}
	if seq > chunks {
		return ErrChunkOrder
	}
	if err := s.Rec.write(id, seq, data); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE remote_sessions SET recording_chunks = recording_chunks + 1,
		recording_bytes = recording_bytes + $2, recording_mime = coalesce(recording_mime, $3) WHERE id = $1`, id, len(data), nz(mime)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Finish marks the upload as complete.
func (s *Service) Finish(ctx context.Context, id int64) error {
	tag, err := s.DB.Exec(ctx, `UPDATE remote_sessions SET recording_status = $2 WHERE id = $1 AND recording_status = $3`, id, RecComplete, RecRecording)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotRecording
	}
	return s.AddEvent(ctx, id, "recording.finished", nil)
}

// WriteRecording decrypts the chunks of a session in order and writes them to w.
func (s *Service) WriteRecording(ctx context.Context, sess *Session, w io.Writer) error {
	if !sess.Recorded() {
		return ErrNoRecording
	}
	for i := 0; i < sess.RecChunks; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := s.Rec.read(sess.ID, i)
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// overhead is what encryption adds to every chunk file (nonce + GCM tag).
func (st *Store) overhead() int64 { return int64(st.aead.NonceSize() + st.aead.Overhead()) }

// Reader is a decrypting io.ReadSeeker over a recording, so it can be served with
// http.ServeContent and browsers can seek with Range requests. It keeps one decrypted
// chunk in memory.
type Reader struct {
	st     *Store
	id     int64
	starts []int64 // plaintext offset of each chunk, plus the total size at the end
	pos    int64
	cur    int
	buf    []byte
}

// OpenRecording returns a Reader for the recording of sess.
func (s *Service) OpenRecording(sess *Session) (*Reader, error) {
	if !sess.Recorded() {
		return nil, ErrNoRecording
	}
	r := &Reader{st: s.Rec, id: sess.ID, starts: make([]int64, 0, sess.RecChunks+1), cur: -1}
	var off int64
	for i := 0; i < sess.RecChunks; i++ {
		fi, err := os.Stat(s.Rec.path(sess.ID, i))
		if err != nil {
			return nil, err
		}
		r.starts = append(r.starts, off)
		off += fi.Size() - s.Rec.overhead()
	}
	r.starts = append(r.starts, off)
	return r, nil
}

// Size is the plaintext length of the recording.
func (r *Reader) Size() int64 { return r.starts[len(r.starts)-1] }

func (r *Reader) Read(p []byte) (int, error) {
	if r.pos >= r.Size() {
		return 0, io.EOF
	}
	// Find the chunk that holds pos.
	i := sort.Search(len(r.starts)-1, func(i int) bool { return r.starts[i+1] > r.pos })
	if i != r.cur {
		b, err := r.st.read(r.id, i)
		if err != nil {
			return 0, err
		}
		r.buf, r.cur = b, i
	}
	n := copy(p, r.buf[r.pos-r.starts[i]:])
	r.pos += int64(n)
	return n, nil
}

func (r *Reader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += r.pos
	case io.SeekEnd:
		off += r.Size()
	default:
		return 0, errors.New("invalid whence")
	}
	if off < 0 {
		return 0, errors.New("negative position")
	}
	r.pos = off
	return off, nil
}

// DeleteRecording removes the video but keeps the session in the audit log.
func (s *Service) DeleteRecording(ctx context.Context, id int64, reason string) error {
	if err := os.RemoveAll(s.Rec.dir(id)); err != nil {
		return err
	}
	if _, err := s.DB.Exec(ctx, `UPDATE remote_sessions SET recording_status = $2, recording_chunks = 0, recording_bytes = 0,
		recording_deleted_at = now() WHERE id = $1 AND recording_status <> 'none'`, id, RecDeleted); err != nil {
		return err
	}
	if _, err := s.DB.Exec(ctx, `UPDATE recording_shares SET revoked_at = now() WHERE session_id = $1 AND revoked_at IS NULL`, id); err != nil {
		return err
	}
	return s.AddEvent(ctx, id, "recording.deleted", map[string]string{"reason": reason})
}

// Cleanup marks uploads that stopped as incomplete, applies the retention period and
// forgets share links that expired a month ago. The scheduler runs it every hour.
func (s *Service) Cleanup(ctx context.Context) error {
	// A viewer that closed without calling Finish leaves the recording open.
	if _, err := s.DB.Exec(ctx, `UPDATE remote_sessions SET recording_status = $1
		WHERE recording_status = $2 AND (ended_at < now() - interval '15 minutes' OR started_at < now() - interval '2 days')`,
		RecIncomplete, RecRecording); err != nil {
		return err
	}
	pol, err := s.Policy(ctx)
	if err != nil {
		return err
	}
	if pol.RetentionDays > 0 {
		rows, err := s.DB.Query(ctx, `SELECT id FROM remote_sessions WHERE recording_status IN ('complete','incomplete')
			AND started_at < $1`, time.Now().AddDate(0, 0, -pol.RetentionDays))
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			if err := s.DeleteRecording(ctx, id, "retention"); err != nil {
				slog.Error("delete recording", "session", id, "err", err)
			}
		}
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM recording_shares WHERE expires_at < now() - interval '30 days'`)
	return err
}
