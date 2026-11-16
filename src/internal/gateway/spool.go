package gateway

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// Person is one hosted install: everything about it lives under Dir.
type Person struct {
	Organization string
	Dir          string

	// Label is what the fleet manager's install list shows in the hostname column.
	Label string
}

func (p Person) Spool() string { return filepath.Join(p.Dir, "spool") }

var errTooManyPeople = errors.New("gateway: the organization is at its max_people")

// openName is the file being appended to. The catalog globs *.jsonl, so the pipeline never reads it.
const openName = "open.part"

const sealedLayout = "20060102T150405.000000000Z"

// spool owns <data_dir>/orgs/<organization>/people/<hash>/spool. Each agent directory holds one
// open file and the sealed, immutable JSONL files cut from it, one per ship; identity.jsonl sits
// beside them.
type spool struct {
	root string
	now  func() time.Time

	mu     sync.Mutex
	people map[string]*personState
	count  map[string]int

	// aliases maps, per organization, every key seen for a person to that person's directory.
	aliases map[string]map[string]string
}

type personState struct {
	person Person
	mu     sync.Mutex
	claims map[string]bool
	dirty  bool
}

func openSpool(root string) (*spool, error) {
	s := &spool{root: root, now: time.Now, people: map[string]*personState{}, count: map[string]int{},
		aliases: map[string]map[string]string{}}
	// What a previous process received and did not ship is dirty again.
	dirs, err := filepath.Glob(filepath.Join(root, "orgs", "*", "people", "*"))
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		org := filepath.Base(filepath.Dir(filepath.Dir(dir)))
		label, _ := os.ReadFile(filepath.Join(dir, "label"))
		pending, _ := filepath.Glob(filepath.Join(dir, "spool", "*", "*"))
		st := &personState{person: Person{Organization: org, Dir: dir, Label: string(label)}, dirty: len(pending) > 0}
		st.claims = readClaims(filepath.Join(dir, "spool", "identity.jsonl"))
		s.people[dir] = st
		s.count[org]++
		if s.aliases[org] == nil {
			aliases := map[string]string{}
			if raw, err := os.ReadFile(s.aliasFile(org)); err == nil {
				if err := json.Unmarshal(raw, &aliases); err != nil {
					return nil, fmt.Errorf("gateway: %s: %w", s.aliasFile(org), err)
				}
			}
			s.aliases[org] = aliases
		}
	}
	return s, nil
}

// personKeys lists what an event's claims can be recognised by, strongest first. The email leads
// so one person's Claude Code and Codex share an install; a host name is not here, since a host
// can hold several people.
func personKeys(agent string, claims map[string]any) []string {
	var keys []string
	if email, _ := claims["user.email"].(string); email != "" {
		keys = append(keys, "email:"+strings.ToLower(email))
	}
	for _, k := range []string{"user.account_uuid", "user.account_id", "user.id"} {
		if v, _ := claims[k].(string); v != "" {
			keys = append(keys, agent+":"+k+":"+v)
		}
	}
	return keys
}

// hostKey names the person of an event that says a session but not who: the machine, or nobody.
func hostKey(agent string, claims map[string]any) string {
	if host, _ := claims["host.name"].(string); host != "" {
		return agent + ":host.name:" + host
	}
	return agent + ":anonymous"
}

// person resolves keys to a hosted install, creating it under the first key if none is known.
// Every key then names that person: Codex puts the email on some events and only an account id
// on others, and both must land in one install.
func (s *spool) person(t *Tenant, keys []string) (*personState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	aliases := s.aliases[t.Organization]
	if aliases == nil {
		aliases = map[string]string{}
		s.aliases[t.Organization] = aliases
	}
	var st *personState
	for _, k := range keys {
		if st = s.people[aliases[k]]; st != nil {
			break
		}
	}
	if st == nil {
		if s.count[t.Organization] >= t.MaxPeople {
			return nil, errTooManyPeople
		}
		sum := sha256.Sum256([]byte(t.Organization + "\x00" + keys[0]))
		dir := filepath.Join(s.root, "orgs", t.Organization, "people", hex.EncodeToString(sum[:16]))
		_, label, _ := strings.Cut(keys[0], ":")
		label = "otel: " + label
		if err := platform.EnsureDir(filepath.Join(dir, "spool"), 0o700); err != nil {
			return nil, err
		}
		if err := platform.WriteAtomic(filepath.Join(dir, "label"), []byte(label), 0o600); err != nil {
			return nil, err
		}
		st = &personState{person: Person{Organization: t.Organization, Dir: dir, Label: label}, claims: map[string]bool{}}
		s.people[dir] = st
		s.count[t.Organization]++
	}
	learned := false
	for _, k := range keys {
		if aliases[k] == "" {
			aliases[k], learned = st.person.Dir, true
		}
	}
	if learned {
		if err := platform.WriteJSON(s.aliasFile(t.Organization), aliases, 0o600); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func (s *spool) aliasFile(organization string) string {
	return filepath.Join(s.root, "orgs", organization, "aliases.json")
}

func readClaims(path string) map[string]bool {
	seen := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return seen
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var line struct {
			Agent  string         `json:"agent"`
			Claims map[string]any `json:"claims"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil {
			seen[claimID(line.Agent, line.Claims)] = true
		}
	}
	return seen
}

func claimID(agent string, claims map[string]any) string {
	raw, _ := json.Marshal(claims)
	return agent + "\x00" + string(raw)
}

// append writes one request's lines for one person and agent, and each claim set that is new.
func (s *spool) append(st *personState, agent string, claimSets []map[string]any, lines [][]byte) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := s.now().UTC()
	dir := filepath.Join(st.person.Spool(), agent)
	if err := platform.EnsureDir(dir, 0o700); err != nil {
		return err
	}
	if err := appendFile(filepath.Join(dir, openName), bytes.Join(lines, []byte("\n"))); err != nil {
		return err
	}
	for _, claims := range claimSets {
		id := claimID(agent, claims)
		if st.claims[id] || len(claims) == 0 {
			continue
		}
		line, err := json.Marshal(map[string]any{"v": 1, "agent": agent, "seen_at": now.Format(time.RFC3339), "claims": claims})
		if err != nil {
			return err
		}
		if err := appendFile(filepath.Join(st.person.Spool(), "identity.jsonl"), line); err != nil {
			return err
		}
		st.claims[id] = true
	}
	st.dirty = true
	return nil
}

func appendFile(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// takeDirty returns the people with unshipped lines and clears their mark; a failed ship marks again.
func (s *spool) takeDirty() []*personState {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*personState
	for _, st := range s.people {
		st.mu.Lock()
		if st.dirty {
			st.dirty = false
			out = append(out, st)
		}
		st.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].person.Dir < out[j].person.Dir })
	return out
}

// seal cuts every open file into a sealed one and returns all sealed files, including those an
// earlier failed ship left. Appends wait on the rename only, never on an upload.
func (s *spool) seal(st *personState) ([]string, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	open, err := filepath.Glob(filepath.Join(st.person.Spool(), "*", openName))
	if err != nil {
		return nil, err
	}
	name := s.now().UTC().Format(sealedLayout) + ".jsonl"
	for _, path := range open {
		if err := os.Rename(path, filepath.Join(filepath.Dir(path), name)); err != nil {
			return nil, fmt.Errorf("gateway: seal %s: %w", path, err)
		}
	}
	return filepath.Glob(filepath.Join(st.person.Spool(), "*", "*.jsonl"))
}

// reap deletes what a successful ship carried.
func (s *spool) reap(sealed []string) error {
	for _, path := range sealed {
		if err := platform.RemoveFile(path); err != nil {
			return fmt.Errorf("gateway: reap %s: %w", path, err)
		}
	}
	return nil
}
