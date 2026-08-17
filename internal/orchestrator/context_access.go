package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"altv1/internal/store"
	"altv1/internal/tooling"
)

var contextReferencePattern = regexp.MustCompile(`alt://context/records/[0-9A-Fa-f-]{36}`)
var contextArtifactPattern = regexp.MustCompile(`alt-tool-output://[0-9A-Fa-f]{64}`)

func (r *sessionRuntime) archiveToolOutput(ctx context.Context, owner, reference string, content []byte) error {
	if !tooling.IsToolOutputReference(reference) {
		return fmt.Errorf("invalid exact tool-output reference")
	}
	return r.store.ArchiveContextArtifact(ctx, store.ContextArtifact{
		Reference: reference, SessionID: r.session.ID, Owner: owner,
		Content: append([]byte(nil), content...),
	})
}

func (r *sessionRuntime) searchContext(
	ctx context.Context,
	owner string,
	input tooling.ContextSearchInput,
) (tooling.ContextSearchResult, error) {
	scope, err := r.contextScope(ctx, owner)
	if err != nil {
		return tooling.ContextSearchResult{}, err
	}
	matches, err := r.store.SearchContextInScope(ctx, scope, input.Query, input.Limit)
	if err != nil {
		return tooling.ContextSearchResult{}, err
	}
	result := tooling.ContextSearchResult{Matches: make([]tooling.ContextSearchMatch, 0, len(matches))}
	for _, match := range matches {
		result.Matches = append(result.Matches, tooling.ContextSearchMatch{
			Reference: match.Reference, SessionID: match.SessionID, SourceSequence: match.SourceSequence,
			Kind: string(match.Kind), Actor: match.Actor,
			CorrelationID: match.CorrelationID, Preview: match.Preview,
		})
	}
	return result, nil
}

func (r *sessionRuntime) browseContext(
	ctx context.Context,
	owner string,
	input tooling.ContextBrowseInput,
) (tooling.ContextBrowseResult, error) {
	scope, err := r.contextScope(ctx, owner)
	if err != nil {
		return tooling.ContextBrowseResult{}, err
	}
	page, err := r.store.BrowseContextInScope(ctx, scope, input.Cursor, input.Limit)
	if err != nil {
		return tooling.ContextBrowseResult{}, err
	}
	result := tooling.ContextBrowseResult{NextCursor: page.NextCursor}
	for _, record := range page.Records {
		result.Records = append(result.Records, tooling.ContextSearchMatch{
			Reference: record.Reference, SessionID: record.SessionID,
			SourceSequence: record.SourceSequence, Kind: string(record.Kind),
			Actor: record.Actor, CorrelationID: record.CorrelationID,
			Preview: record.Preview,
		})
	}
	return result, nil
}

func (r *sessionRuntime) openContext(
	ctx context.Context,
	owner string,
	input tooling.ContextOpenInput,
) (tooling.ContextOpenResult, error) {
	scope, err := r.contextScope(ctx, owner)
	if err != nil {
		return tooling.ContextOpenResult{}, err
	}
	if tooling.IsToolOutputReference(input.Reference) {
		artifact, err := r.store.ContextArtifactInScope(ctx, scope, input.Reference)
		if err != nil {
			return tooling.ContextOpenResult{}, err
		}
		result := tooling.ContextOpenResult{
			Reference: artifact.Reference, SessionID: artifact.SessionID,
			OccurredAt: artifact.CreatedAt.Format(time.RFC3339Nano), Kind: "tool.output.archive",
			Actor: artifact.Owner, Digest: artifact.Digest,
		}
		return boundedContextOpen(result, artifact.Content, input.ByteOffset, input.MaxBytes)
	}
	record, err := r.store.ContextRecordInScope(ctx, scope, input.Reference)
	if err != nil {
		return tooling.ContextOpenResult{}, err
	}
	result := tooling.ContextOpenResult{
		Reference: record.Reference(), SessionID: record.SessionID,
		SourceSequence: record.SourceSequence, OccurredAt: record.CreatedAt.Format(time.RFC3339Nano),
		Kind: string(record.Kind), Actor: record.Actor,
		CorrelationID: record.CorrelationID, CausationID: record.CausationID, Digest: record.Digest,
	}
	return boundedContextOpen(result, record.Content, input.ByteOffset, input.MaxBytes)
}

func boundedContextOpen(
	result tooling.ContextOpenResult,
	content []byte,
	offset int,
	limit int,
) (tooling.ContextOpenResult, error) {
	if offset < 0 || offset > len(content) {
		return tooling.ContextOpenResult{}, fmt.Errorf("context byte_offset must be within [0,%d]", len(content))
	}
	if utf8.Valid(content) && offset < len(content) && !utf8.RuneStart(content[offset]) {
		return tooling.ContextOpenResult{}, fmt.Errorf("context byte_offset %d splits a UTF-8 code point", offset)
	}
	end := len(content)
	if limit > 0 && limit < len(content)-offset {
		end = offset + limit
	}
	if utf8.Valid(content) {
		for end > offset && end < len(content) && !utf8.RuneStart(content[end]) {
			end--
		}
		if end == offset && offset < len(content) {
			return tooling.ContextOpenResult{}, fmt.Errorf("context max_bytes is too small for the next UTF-8 code point")
		}
	}
	chunk := content[offset:end]
	digest := sha256.Sum256(chunk)
	result.ByteCount = len(content)
	result.ByteStart = offset
	result.ByteEnd = end
	result.HasMore = end < len(content)
	if result.HasMore {
		result.NextByteOffset = end
	}
	result.ChunkDigest = hex.EncodeToString(digest[:])
	if utf8.Valid(chunk) {
		result.Encoding = "utf-8"
		result.Content = string(chunk)
	} else {
		result.Encoding = "base64"
		result.Content = base64.StdEncoding.EncodeToString(chunk)
	}
	return result, nil
}

func (r *sessionRuntime) contextScope(ctx context.Context, owner string) (store.ContextScope, error) {
	owner = strings.TrimSpace(owner)
	parts := strings.Split(owner, ":")
	if len(parts) < 2 {
		return store.ContextScope{}, fmt.Errorf("invalid context owner %q", owner)
	}

	// Leadership-capable agents and peers are context-bearing. They can inspect the
	// exact ledger for this conversation, while every other conversation remains
	// outside the authority boundary.
	switch parts[0] {
	case "agent":
		turns, err := r.store.ConversationSessions(ctx, r.session.ID)
		if err != nil {
			return store.ContextScope{}, err
		}
		sessions := make([]string, 0, len(turns))
		for _, turn := range turns {
			sessions = append(sessions, turn.ID)
		}
		return store.ContextScope{
			SessionIDs: sessions, IncludeAllSessionRecords: true,
		}, nil
	}

	state, err := r.projection(ctx)
	if err != nil {
		return store.ContextScope{}, err
	}
	turns, err := r.store.ConversationSessions(ctx, r.session.ID)
	if err != nil {
		return store.ContextScope{}, err
	}
	sessionIDs := make([]string, 0, len(turns))
	for _, turn := range turns {
		sessionIDs = append(sessionIDs, turn.ID)
	}
	scope := store.ContextScope{SessionIDs: sessionIDs}
	switch parts[0] {
	case "specialist":
		if len(parts) < 3 {
			return store.ContextScope{}, fmt.Errorf("invalid specialist context owner %q", owner)
		}
		delegation := state.Delegations[parts[2]]
		if delegation == nil || delegation.Spec.SpecialistID != parts[1] {
			return store.ContextScope{}, fmt.Errorf("specialist context owner is not assigned to %s", parts[2])
		}
		// Do not grant the delegation correlation: it also contains records from
		// earlier attempts of this delegation. Every retry is a new clean-slate
		// specialist invocation. The exact attempt owner permits only archives
		// created inside this invocation; all other records must be cited
		// explicitly by the caller in the standalone prompt.
		scope.Owners = []string{owner}
		scope.RecordIDs = explicitContextRecordIDs(delegation.Spec.Objective, delegation.Spec.Context)
		scope.ArtifactReferences = explicitContextArtifactReferences(delegation.Spec.Objective, delegation.Spec.Context)
	case "peer":
		if len(parts) < 3 {
			return store.ContextScope{}, fmt.Errorf("invalid peer context owner %q", owner)
		}
		collaborationID := parts[2]
		for _, turn := range state.SortedPeerTurns() {
			if turn.Spec.CollaborationID != collaborationID || turn.Spec.PeerID != parts[1] {
				continue
			}
			scope.CorrelationIDs = append(scope.CorrelationIDs, turn.Spec.ID)
			scope.RecordIDs = append(scope.RecordIDs,
				explicitContextRecordIDs(turn.Spec.Objective, turn.Spec.Context)...,
			)
			scope.ArtifactReferences = append(scope.ArtifactReferences,
				explicitContextArtifactReferences(turn.Spec.Objective, turn.Spec.Context)...,
			)
			scope.Owners = append(scope.Owners,
				fmt.Sprintf("peer:%s:%s:%d:%d", turn.Spec.PeerID, collaborationID, turn.Spec.Round, turn.Attempt),
			)
		}
		if len(scope.CorrelationIDs) == 0 {
			return store.ContextScope{}, fmt.Errorf("peer collaboration %s is unavailable to %s", collaborationID, parts[1])
		}
		// The validation above prevents a fabricated peer owner from opening the
		// ledger. A real peer has the same conversation-bearing context authority
		// as a leadership-capable agent, including work from earlier consultations.
		scope.IncludeAllSessionRecords = true
	default:
		return store.ContextScope{}, fmt.Errorf("unknown context owner %q", owner)
	}
	return scope, nil
}

func explicitContextArtifactReferences(texts ...string) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, text := range texts {
		for _, reference := range contextArtifactPattern.FindAllString(text, -1) {
			if _, exists := seen[reference]; exists {
				continue
			}
			seen[reference] = struct{}{}
			result = append(result, reference)
		}
	}
	return result
}

func explicitContextRecordIDs(texts ...string) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, text := range texts {
		for _, reference := range contextReferencePattern.FindAllString(text, -1) {
			id, err := store.ContextRecordID(reference)
			if err != nil {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			result = append(result, id)
		}
	}
	return result
}
