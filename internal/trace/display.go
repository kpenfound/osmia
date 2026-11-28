package trace

// ManagerDisplay is the name of the manager role wherever a person, an agent
// or an API client sees it: API responses, CLI output, the web page, prompts
// and new telemetry. The role's stored name is Manager.
const ManagerDisplay = "manager"

// DisplayRole maps a stored role name, agent ID, actor ID or decider to the
// name shown outside the service. The manager's stored name becomes
// ManagerDisplay and every other value is returned as it is. Stored records
// keep the stored name; only values that leave the service are mapped.
func DisplayRole(stored string) string {
	if stored == Manager {
		return ManagerDisplay
	}
	return stored
}

// DisplayRoles maps each of roles with DisplayRole and returns a new slice.
func DisplayRoles(roles []string) []string {
	if roles == nil {
		return nil
	}
	out := make([]string, len(roles))
	for i, role := range roles {
		out[i] = DisplayRole(role)
	}
	return out
}

// StoredRole maps a role name a client sent to the name stored records use. It
// inverts DisplayRole, and a client that still sends the stored name gets it
// back unchanged.
func StoredRole(shown string) string {
	if shown == ManagerDisplay {
		return Manager
	}
	return shown
}

// Display returns the actor as it is shown outside the service: the manager's
// stored ID becomes ManagerDisplay.
func (a Actor) Display() Actor {
	a.ID = DisplayRole(a.ID)
	return a
}
