package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
)

// keyringService is the name the OS keychain keeps the tool's entries under; each entry's user is a base URL.
const keyringService = "verifyinboxes-cli"

// refreshEarly is how long before its end an access token is renewed.
const refreshEarly = 60 * time.Second

// OAuthCredential is a saved sign-in: the client the tool registered, and its tokens.
type OAuthCredential struct {
	ClientID     string    `json:"client_id"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// savedEntry is what is saved for one base URL: an API key, the sign-in for the API (/v1) and the one for AI
// apps (/mcp).
type savedEntry struct {
	APIKey string           `json:"api_key,omitempty"`
	API    *OAuthCredential `json:"api,omitempty"`
	MCP    *OAuthCredential `json:"mcp,omitempty"`
}

func (e savedEntry) empty() bool { return e.APIKey == "" && e.API == nil && e.MCP == nil }

// credStore keeps what is saved: in the OS keychain (macOS Keychain, Windows Credential Manager, the Secret Service
// on Linux) or, where there is none or VERIFYINBOXES_CONFIG_DIR is set, in credentials.json in the configuration
// directory, readable by you alone.
type credStore struct {
	// file is credentials.json; "" when there is no configuration directory.
	file string
	// keychain says the OS keychain is tried first.
	keychain bool
	// keychainWorked says the keychain has read or saved an entry in this run. From then on a failing call is an
	// error, never a quiet switch to the file: the keychain's older entry would be read first by the next run (a
	// renewed sign-in saved to the file only would be lost, its refresh token used up). A keychain that found nothing
	// does not count: with no entry of its own to hide the file, a save it refuses goes to the file.
	keychainWorked bool
	// notice is told the file's path the first time it is written.
	notice func(path string)

	mu sync.Mutex
}

func (a *App) store() (*credStore, error) {
	if a.creds != nil {
		return a.creds, nil
	}
	s := &credStore{notice: func(path string) {
		a.note("There is no keychain on this computer: what you save is kept in %s, readable by you alone.", path)
	}}
	file, chosen := credentialsFile()
	s.file, s.keychain = file, !chosen
	if chosen {
		s.notice = func(path string) {
			a.note("What you save is kept in %s (%s), readable by you alone.", path, envConfigDir)
		}
	}
	a.creds = s
	return s, nil
}

func storeError(format string, args ...any) *cliError {
	return &cliError{msg: fmt.Sprintf(format, args...), exit: exitError,
		hint: "Set " + envConfigDir + " to a directory you can write to, or use " + envAPIKey + "."}
}

// keychainError is a keychain that failed after it had worked in this run.
func keychainError(what, base string, err error) *cliError {
	return &cliError{msg: fmt.Sprintf("the keychain could not %s what is saved for %s: %v", what, base, err), exit: exitError,
		hint: "Unlock the keychain and try again, or set " + envConfigDir + " to keep what you save in a file instead."}
}

// load returns what is saved for base (an empty entry when nothing is).
func (s *credStore) load(base string) (savedEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(base)
}

func (s *credStore) loadLocked(base string) (savedEntry, error) {
	if s.keychain {
		secret, err := keyring.Get(keyringService, base)
		switch {
		case err == nil:
			s.keychainWorked = true
			var e savedEntry
			if json.Unmarshal([]byte(secret), &e) != nil {
				return savedEntry{}, storeError("what the keychain holds for %s is not readable: run `verifyinboxes logout`, then sign in again", base)
			}
			return e, nil
		case errors.Is(err, keyring.ErrNotFound):
			// Nothing saved there: whether the keychain can save is known only once it has.
		case s.keychainWorked:
			return savedEntry{}, keychainError("read", base, err)
		default:
			s.keychain = false // none here: the file it is
		}
	}
	all, err := s.readFile()
	if err != nil {
		return savedEntry{}, err
	}
	return all[base], nil
}

// update changes what is saved for base, holding the lock other runs of the tool take, so that a change made by
// another one meanwhile (a renewed token) is never written over.
func (s *credStore) update(base string, change func(*savedEntry)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	e, err := s.loadLocked(base)
	if err != nil {
		return err
	}
	change(&e)
	return s.saveLocked(base, e)
}

func (s *credStore) saveLocked(base string, e savedEntry) error {
	if s.keychain {
		var err error
		if e.empty() {
			err = keyring.Delete(keyringService, base)
			if errors.Is(err, keyring.ErrNotFound) {
				err = nil
			}
		} else {
			data, _ := json.Marshal(e)
			err = keyring.Set(keyringService, base, string(data))
		}
		if err == nil {
			if !e.empty() {
				s.keychainWorked = true // it holds the entry now
			}
			// Anything saved in the file while there was no keychain goes (as far as it can: the keychain has it).
			_ = s.writeEntry(base, savedEntry{}, false)
			return nil
		}
		if s.keychainWorked {
			return keychainError("save", base, err)
		}
		s.keychain = false
	}
	return s.writeEntry(base, e, true)
}

// remove deletes what is saved for base, wherever it is.
func (s *credStore) remove(base string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if s.keychain {
		switch err := keyring.Delete(keyringService, base); {
		case err == nil || errors.Is(err, keyring.ErrNotFound):
			// Deleted, or nothing to delete: either way the keychain holds no entry to hide the file.
		case s.keychainWorked:
			return keychainError("delete", base, err)
		default:
			s.keychain = false
		}
	}
	return s.writeEntry(base, savedEntry{}, false)
}

func (s *credStore) readFile() (map[string]savedEntry, error) {
	all := map[string]savedEntry{}
	if s.file == "" {
		return all, nil
	}
	data, err := os.ReadFile(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return all, nil
	}
	if err != nil {
		return nil, storeError("cannot read %s: %v", s.file, err)
	}
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, storeError("%s is not readable: delete it, then sign in again", s.file)
	}
	return all, nil
}

// writeEntry sets (or, for an empty entry, deletes) base's entry in the file. create: the file may be made.
func (s *credStore) writeEntry(base string, e savedEntry, create bool) error {
	if s.file == "" {
		if !create {
			return nil
		}
		return storeError("there is no keychain and no configuration directory to save to")
	}
	all, err := s.readFile()
	if err != nil {
		return err
	}
	_, existed := all[base]
	if e.empty() {
		if !existed {
			return nil
		}
		delete(all, base)
	} else {
		all[base] = e
	}
	_, statErr := os.Stat(s.file)
	isNew := errors.Is(statErr, os.ErrNotExist)
	if isNew && len(all) == 0 {
		return nil
	}
	data, _ := json.MarshalIndent(all, "", "  ")
	if err := writeAtomically(s.file, append(data, '\n')); err != nil {
		return storeError("cannot save to %s: %v", s.file, err)
	}
	if isNew && s.notice != nil {
		s.notice(s.file)
	}
	return nil
}

// writeAtomically writes path so that only its owner can read it (its directory 0700, the file 0600): a
// temporary file beside it, then a rename, so that a reader never sees half of it.
func writeAtomically(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil && !isWindows() {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// A run of the tool holds the lock beside credentials.json only around one read and one write of what is saved
// (with the keychain, that may wait on its unlock prompt), never across a request.
var (
	// lockWait is how long a run waits for a lock another run holds. Then it gives up, with an error: it never goes
	// on without the lock.
	lockWait = 15 * time.Second
	// lockStale is how old a lock must be to be taken over as one a run that died left behind: longer than a renewal's
	// request may take (oauthHTTP's timeout), so that no live run's lock is taken.
	lockStale = 90 * time.Second
)

// lock takes the lock file beside credentials.json, which other runs of the tool take too, and returns its release.
// The file holds a random token of this run's, and the release removes it only while it still holds that token: a
// run whose lock was taken over never removes the lock of the run that took it. Without a directory for it (or one
// the tool cannot write to, where nothing can be saved in a file either), nothing is locked.
func (s *credStore) lock() (func(), error) {
	if s.file == "" {
		return func() {}, nil
	}
	path := s.file + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return func() {}, nil
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, storeError("cannot make a lock token: %v", err)
	}
	token := hex.EncodeToString(raw[:])
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := f.WriteString(token)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				_ = os.Remove(path)
				return nil, storeError("cannot write the lock %s: %v", path, werr)
			}
			return func() { releaseLock(path, token) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return func() {}, nil
		}
		held, heldErr := os.ReadFile(path)
		if info, err := os.Stat(path); heldErr == nil && err == nil && time.Since(info.ModTime()) > lockStale {
			// Left by a run that died: taken over, unless another run took it over a moment ago (its token is new). One
			// that cannot be removed (a directory that has become read-only) is an error, not a loop that never waits.
			if again, err := os.ReadFile(path); err == nil && string(again) == string(held) {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return nil, storeError("cannot remove the lock %s, left by a run that died: %v", path, err)
				}
			}
			if time.Now().Before(deadline) {
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, &cliError{msg: fmt.Sprintf("another run of verifyinboxes has held %s for over %v", path, lockWait),
				exit: exitError, hint: "Try again once it is done. If no other is running, delete that file."}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// releaseLock removes the lock file at path while it holds token, the lock of the run that took it.
func releaseLock(path, token string) {
	if held, err := os.ReadFile(path); err == nil && string(held) == token {
		_ = os.Remove(path)
	}
}

// ---- a sign-in as the SDK's token source --------------------------------------------------------------------------

// resourceKind is which of the API's two resources a sign-in is for.
type resourceKind string

const (
	forAPI resourceKind = "api"
	forMCP resourceKind = "mcp"
)

func (k resourceKind) path() string {
	if k == forMCP {
		return "/mcp"
	}
	return "/v1"
}

func (k resourceKind) loginCommand() string {
	if k == forMCP {
		return "verifyinboxes login --for mcp"
	}
	return "verifyinboxes login"
}

func (k resourceKind) of(e *savedEntry) **OAuthCredential {
	if k == forMCP {
		return &e.MCP
	}
	return &e.API
}

// signIn is a saved sign-in as a verifyinboxes.TokenSource and Refresher. Its access token is renewed with the
// refresh token a minute before it ends, and when the API refuses it (invalid_token); the renewed one is saved
// before it is used. Before renewing it reads the store again: another run of the tool may have renewed it already,
// and a refresh token is used once.
type signIn struct {
	app  *App
	base string
	kind resourceKind
	auth *authServer

	mu   sync.Mutex
	cred OAuthCredential
}

func (a *App) signIn(base string, kind resourceKind, cred OAuthCredential) *signIn {
	return &signIn{app: a, base: base, kind: kind, cred: cred}
}

// Token is the access token to send, renewed first when it ends within a minute.
func (s *signIn) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Until(s.cred.ExpiresAt) > refreshEarly {
		return s.cred.AccessToken, nil
	}
	if err := s.renew(ctx, s.cred.AccessToken); err != nil {
		return "", err
	}
	return s.cred.AccessToken, nil
}

// Refresh renews the access token the API refused.
func (s *signIn) Refresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renew(ctx, s.cred.AccessToken)
}

// saved is the sign-in as the store holds it now; nil when there is none.
func (s *signIn) saved() *OAuthCredential {
	store, err := s.app.store()
	if err != nil {
		return nil
	}
	e, err := store.load(s.base)
	if err != nil {
		return nil
	}
	return *s.kind.of(&e)
}

// fresh reports whether c is a token other than refused with more than a minute left.
func fresh(c *OAuthCredential, refused string) bool {
	return c != nil && c.AccessToken != refused && time.Until(c.ExpiresAt) > refreshEarly
}

// renew replaces the access token refused (ending, or refused by the API).
func (s *signIn) renew(ctx context.Context, refused string) error {
	if saved := s.saved(); saved != nil {
		if fresh(saved, refused) {
			s.cred = *saved // another run renewed it, or signed in again
			return nil
		}
		s.cred = *saved // its refresh token is the newest
	}
	for attempt := 0; attempt < 2; attempt++ {
		used := s.cred.RefreshToken
		tokens, err := s.refreshWith(ctx, used)
		if err == nil {
			next := OAuthCredential{ClientID: s.cred.ClientID, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
				ExpiresAt: time.Now().Add(tokens.lifetime())}
			store, err := s.app.store()
			if err == nil {
				err = store.update(s.base, func(e *savedEntry) { *s.kind.of(e) = &next })
			}
			if err != nil {
				return &cliError{msg: "the renewed sign-in could not be saved: " + err.Error(), exit: exitError,
					hint: "Sign in again: " + s.kind.loginCommand()}
			}
			s.cred = next
			return nil
		}
		var refusedGrant *oauthError
		if !errors.As(err, &refusedGrant) || refusedGrant.Code != "invalid_grant" {
			return err
		}
		// Another run of the tool may have used this refresh token a moment ago, and be saving what it got.
		newer := s.waitForNewer(ctx, used)
		if newer == nil {
			return signInEnded(s.kind, refusedGrant)
		}
		s.cred = *newer
		if fresh(newer, refused) {
			return nil
		}
	}
	return &cliError{msg: "your sign-in could not be renewed: sign in again: " + s.kind.loginCommand(), exit: exitError}
}

// signInEnded is the error of a sign-in whose refresh token the server refused (invalid_grant): it says why when
// the server did.
func signInEnded(kind resourceKind, refused *oauthError) *cliError {
	msg := "your sign-in has ended"
	if why := clean(refused.Description); why != "" {
		msg += " (" + why + ")"
	}
	return &cliError{msg: msg + ": sign in again: " + kind.loginCommand(), requestID: refused.RequestID, exit: exitError}
}

// waitForNewer reads the store a few times over two seconds for a refresh token other than used.
func (s *signIn) waitForNewer(ctx context.Context, used string) *OAuthCredential {
	for _, pause := range []time.Duration{0, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second} {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pause):
		}
		if saved := s.saved(); saved != nil && saved.RefreshToken != used && saved.RefreshToken != "" {
			return saved
		}
	}
	return nil
}

// refreshWith is the refresh_token grant for this sign-in's resource.
func (s *signIn) refreshWith(ctx context.Context, refreshToken string) (*tokenAnswer, error) {
	if s.auth == nil {
		auth, err := s.app.discover(ctx, s.base)
		if err != nil {
			return nil, err
		}
		s.auth = auth
	}
	return s.auth.token(ctx, map[string]string{
		"grant_type": "refresh_token", "refresh_token": refreshToken, "client_id": s.cred.ClientID,
		"resource": s.base + s.kind.path(),
	})
}
