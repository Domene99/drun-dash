package httpapi

// SameOrg reports whether a row loaded by id belongs to the caller's
// organization, implementing the ADR-0002 §1 tenant-isolation check.
//
// The store's by-id lookups (GetByID) key on the primary key alone and are not
// org-scoped, so a caller could reference another org's UUID. After loading a
// row by id, a handler compares the row's OrgID (or, for nested resources, the
// owning parent's OrgID) to the caller's org from the token:
//
//	row, err := s.GetByID(ctx, id)
//	if err != nil {
//		httpapi.WriteStoreError(w, err)
//		return
//	}
//	if !httpapi.SameOrg(caller.OrgID, row.OrgID) {
//		httpapi.WriteNotFound(w, "resource not found")
//		return
//	}
//
// Contract: on a mismatch the handler returns 404 not_found, never 403.
// Returning 404 avoids leaking the existence of a resource in another tenant —
// a cross-org id reference is made indistinguishable from "does not exist".
// This applies to every by-id read, update, and delete; there is no
// "read is harmless" exception.
func SameOrg(callerOrgID, rowOrgID string) bool {
	return callerOrgID == rowOrgID
}
