package api

import (
	"crypto/rand"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
	"idlifecycle/internal/sources"
	"idlifecycle/internal/store"
)

// ---- naming conflicts -------------------------------------------------------------------

func (s *Server) handleChangeNames(w http.ResponseWriter, r *http.Request) {
	cur, conflicts, sugg, err := s.Engine.ChangeNames(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if conflicts == nil {
		conflicts = []model.NameConflict{}
	}
	if sugg == nil {
		sugg = []engine.NameSuggestion{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"current": cur, "conflicts": conflicts, "suggestions": sugg})
}

// nameTaken writes a 409 with the conflicts next to the error envelope.
func nameTaken(w http.ResponseWriter, nt *engine.NameTakenError) {
	var body struct {
		apiError
		Conflicts []model.NameConflict `json:"conflicts"`
	}
	body.Error.Code, body.Error.Message = "name_taken", "the name is already in use: "+nt.Error()
	body.Conflicts = nt.Conflicts
	writeJSON(w, http.StatusConflict, body)
}

func (s *Server) handleSetChangeName(w http.ResponseWriter, r *http.Request) {
	var in model.Candidate
	if !decode(w, r, &in) {
		return
	}
	c, err := s.Engine.SetChangeName(r.Context(), r.PathValue("id"), in, principal(r).User.Username)
	var nt *engine.NameTakenError
	switch {
	case errors.As(err, &nt):
		nameTaken(w, nt)
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// handleCheckNames: {names:[{field,value,ou}]} -> {taken:[NameConflict]} (AD and the other open creates).
func (s *Server) handleCheckNames(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Names []ad.NameQuery `json:"names"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Names) == 0 || len(in.Names) > 20 {
		badRequest(w, "send 1 to 20 names")
		return
	}
	settings, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Group the names into one candidate per OU (the engine checks a candidate's names together).
	var c model.Candidate
	ou := ""
	for _, n := range in.Names {
		v := strings.TrimSpace(n.Value)
		if v == "" || len(v) > 256 || strings.ContainsAny(v, "\r\n\x00") {
			badRequest(w, "every name needs a value (at most 256 characters, one line)")
			return
		}
		switch n.Field {
		case ad.NameSam:
			c.Sam = v
		case ad.NameUPN:
			c.UPN = v
		case ad.NameMail:
			c.Mail = v
		case ad.NameCN:
			c.CN, ou = v, strings.TrimSpace(n.OU)
		default:
			badRequest(w, "field must be sam, upn, mail or cn")
			return
		}
	}
	taken, err := s.Engine.CheckNames(r.Context(), settings, c, ou, "", "")
	if err != nil {
		writeErr(w, http.StatusBadGateway, "ad", err.Error())
		return
	}
	if taken == nil {
		taken = []model.NameConflict{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"taken": taken})
}

// ---- links ---------------------------------------------------------------------------------

func (s *Server) handleSetChangeLink(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ObjectGUID string `json:"object_guid"`
		CreateNew  bool   `json:"create_new"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.CreateNew && strings.TrimSpace(in.ObjectGUID) == "" {
		badRequest(w, "send object_guid (the account to link) or create_new: true")
		return
	}
	c, err := s.Engine.SetChangeLink(r.Context(), r.PathValue("id"), strings.TrimSpace(in.ObjectGUID), in.CreateNew, principal(r).User.Username)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleSetIdentityLink(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ObjectGUID string `json:"object_guid"`
		Clear      bool   `json:"clear"`
	}
	if !decode(w, r, &in) {
		return
	}
	id, err := s.Engine.SetIdentityLink(r.Context(), r.PathValue("id"), in.ObjectGUID, in.Clear, principal(r).User.Username)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, id)
}

func (s *Server) handleCorrelationReport(w http.ResponseWriter, r *http.Request) {
	rep := engine.CorrelationReport{Uncorrelated: []engine.ReportAccount{}, Verify: []engine.ReportChange{}, NeedsName: []engine.ReportChange{}}
	if _, err := s.Store.GetState(r.Context(), engine.ReportState, &rep); err != nil {
		s.fail(w, r, err)
		return
	}
	settings, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Accounts ignored since the report was written drop out right away.
	kept := rep.Uncorrelated[:0:0]
	for _, a := range rep.Uncorrelated {
		if !engine.IgnoredAccount(settings.Correlate.IgnoreAccounts, a.Sam, a.DN) {
			kept = append(kept, a)
		}
	}
	rep.Uncorrelated = kept
	if rep.RunID == "" {
		rep.Enabled = settings.Correlate.Enabled
	}
	writeJSON(w, http.StatusOK, rep)
}

// ---- manual people ---------------------------------------------------------------------------

type manualSourceInfo struct {
	ID       string              `json:"id"`
	Name     string              `json:"name"`
	Enabled  bool                `json:"enabled"`
	IDPrefix string              `json:"id_prefix"`
	Fields   []sources.FormField `json:"fields"`
}

func manualInfo(src model.Source) (manualSourceInfo, error) {
	text, _ := src.Config["form_fields"].(string)
	if strings.TrimSpace(text) == "" {
		text = sources.DefaultManualFields
	}
	fields, err := sources.ParseFormFields(text)
	prefix, _ := src.Config["id_prefix"].(string)
	return manualSourceInfo{ID: src.ID, Name: src.Name, Enabled: src.Enabled, IDPrefix: strings.TrimSpace(prefix), Fields: fields}, err
}

// manualSource loads {sid} and checks it is a manual source.
func (s *Server) manualSource(w http.ResponseWriter, r *http.Request) (manualSourceInfo, bool) {
	src, err := s.Store.GetSource(r.Context(), r.PathValue("sid"))
	if err != nil {
		s.fail(w, r, err)
		return manualSourceInfo{}, false
	}
	if src.Type != sources.TypeManual {
		badRequest(w, "this source is not a manual-entry source")
		return manualSourceInfo{}, false
	}
	info, err := manualInfo(src)
	if err != nil {
		badRequest(w, "the source's form fields are invalid: "+err.Error())
		return info, false
	}
	return info, true
}

func (s *Server) handleListManualSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.Store.ListSources(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []manualSourceInfo{}
	for _, src := range srcs {
		if src.Type != sources.TypeManual {
			continue
		}
		info, err := manualInfo(src)
		if err != nil {
			info.Fields = []sources.FormField{}
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListManualPeople(w http.ResponseWriter, r *http.Request) {
	info, ok := s.manualSource(w, r)
	if !ok {
		return
	}
	people, err := s.Store.ListManualPeople(r.Context(), info.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ids, err := s.Store.ListIdentitiesBySource(r.Context(), info.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	byExt := map[string]model.Identity{}
	for _, id := range ids {
		byExt[id.ExternalID] = id
	}
	for i := range people {
		if id, ok := byExt[people[i].ExternalID]; ok {
			people[i].State, people[i].ADSam = id.State, id.ADSam
		} else {
			people[i].State = "not synced yet"
		}
	}
	writeJSON(w, http.StatusOK, people)
}

type manualIn struct {
	Attrs map[string]string `json:"attrs"`
	Sync  bool              `json:"sync"`
	Note  string            `json:"note"`
}

// manualRow is the row a sync will see for a person (without the generated id for previews).
func manualRow(attrs map[string]string, ext string) map[string]string {
	row := make(map[string]string, len(attrs)+3)
	for k, v := range attrs {
		row[k] = v
	}
	row[sources.ManualIDField], row[sources.ManualStatusField], row["Source"] = ext, sources.ManualActive, sources.TypeManual
	return row
}

func (s *Server) handlePreviewManualPerson(w http.ResponseWriter, r *http.Request) {
	info, ok := s.manualSource(w, r)
	if !ok {
		return
	}
	var in manualIn
	if !decode(w, r, &in) {
		return
	}
	attrs, err := sources.CheckManualAttrs(info.Fields, in.Attrs)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	prev, err := s.Engine.PreviewPerson(r.Context(), manualRow(attrs, info.IDPrefix+"XXXXXX"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, prev)
}

const idAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomID(prefix string) (string, error) {
	b := make([]byte, 6)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(idAlphabet))))
		if err != nil {
			return "", err
		}
		b[i] = idAlphabet[n.Int64()]
	}
	return prefix + string(b), nil
}

func (s *Server) handleCreateManualPerson(w http.ResponseWriter, r *http.Request) {
	info, ok := s.manualSource(w, r)
	if !ok {
		return
	}
	var in manualIn
	if !decode(w, r, &in) {
		return
	}
	attrs, err := sources.CheckManualAttrs(info.Fields, in.Attrs)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	by := principal(r).User.Username
	now := time.Now().UTC()
	p := model.ManualPerson{ID: uuid.NewString(), SourceID: info.ID, Attrs: attrs, CreatedBy: by, CreatedAt: now, UpdatedBy: by, UpdatedAt: now}
	for try := 0; ; try++ {
		if p.ExternalID, err = randomID(info.IDPrefix); err != nil {
			s.fail(w, r, err)
			return
		}
		err = s.Store.CreateManualPerson(r.Context(), p)
		if !errors.Is(err, store.ErrConflict) || try == 4 {
			break
		}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "manual.create", "manual_person", p.ID, map[string]any{"source": info.Name, "external_id": p.ExternalID, "attrs": attrs})
	out := map[string]any{"id": p.ID, "source_id": p.SourceID, "external_id": p.ExternalID, "attrs": p.Attrs, "ended": false,
		"created_by": p.CreatedBy, "created_at": p.CreatedAt, "updated_by": p.UpdatedBy, "updated_at": p.UpdatedAt}
	if in.Sync {
		if run, _, err := s.Engine.Start(s.BaseCtx, "manual", by); err == nil {
			out["run_id"] = run.ID
		} else {
			out["sync_error"] = err.Error()
		}
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleUpdateManualPerson(w http.ResponseWriter, r *http.Request) {
	info, ok := s.manualSource(w, r)
	if !ok {
		return
	}
	var in manualIn
	if !decode(w, r, &in) {
		return
	}
	p, err := s.Store.GetManualPerson(r.Context(), info.ID, r.PathValue("pid"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	attrs, err := sources.CheckManualAttrs(info.Fields, in.Attrs)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	before := p.Attrs
	p.Attrs, p.UpdatedBy, p.UpdatedAt = attrs, principal(r).User.Username, time.Now().UTC()
	if err := s.Store.UpdateManualPerson(r.Context(), p); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "manual.update", "manual_person", p.ID, map[string]any{"source": info.Name, "external_id": p.ExternalID, "before": before, "after": attrs})
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleEndManualPerson(w http.ResponseWriter, r *http.Request) {
	info, ok := s.manualSource(w, r)
	if !ok {
		return
	}
	var in manualIn
	if !decode(w, r, &in) {
		return
	}
	p, err := s.Store.GetManualPerson(r.Context(), info.ID, r.PathValue("pid"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p.Ended, p.UpdatedBy, p.UpdatedAt = true, principal(r).User.Username, time.Now().UTC()
	if err := s.Store.UpdateManualPerson(r.Context(), p); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "manual.end", "manual_person", p.ID, map[string]any{"source": info.Name, "external_id": p.ExternalID, "note": strings.TrimSpace(in.Note)})
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeleteManualPerson(w http.ResponseWriter, r *http.Request) {
	info, ok := s.manualSource(w, r)
	if !ok {
		return
	}
	p, err := s.Store.GetManualPerson(r.Context(), info.ID, r.PathValue("pid"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ids, err := s.Store.ListIdentitiesBySource(r.Context(), info.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, id := range ids {
		if id.ExternalID == p.ExternalID && id.ADGUID != "" {
			writeErr(w, http.StatusConflict, "conflict", "this person has an AD account ("+id.ADSam+"); end them instead, so the account is disabled through the normal flow")
			return
		}
	}
	if err := s.Store.DeleteManualPerson(r.Context(), info.ID, p.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "manual.delete", "manual_person", p.ID, map[string]any{"source": info.Name, "external_id": p.ExternalID})
	w.WriteHeader(http.StatusNoContent)
}
