package telemetry

// PoolID is the identity of the capacity pool that grants role's slots. A pool
// shared across projects has one identity, so it is the role's name and never
// carries a project: "mason", "reviewer", "committee", "architect" and
// "librarian" for the scheduler's turn pools and workflow reservations, and
// "checks" for the service's check pool. Every span that describes a pool uses
// it, so spans about one pool line up.
func PoolID(role string) string { return role }

// PoolRolesOf lists the roles the pool serves: the role it is named for.
func PoolRolesOf(pool string) []string { return []string{pool} }
