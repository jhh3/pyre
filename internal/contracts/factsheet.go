package contracts

// Project fact sheet (setup review F8): a project document of settled facts,
// each quoting the owner's words, that the hub puts into the launch context
// of every session started in the project. It is an ordinary document
// (slug FactSheetSlug). Only the owner writes it (`pyre fact add`); a
// session proposes a fact (propose_fact) and the hub adds it when the owner
// approves.
const (
	FactSheetSlug  = "facts"
	FactSheetTitle = "Fact sheet"
	// FactSheetMaxBytes bounds the fact sheet in a launch context;
	// StandingContextMaxBytes bounds fact sheet and grants together.
	FactSheetMaxBytes       = 6 << 10
	StandingContextMaxBytes = 8 << 10
)

// FactSheetPrincipal is the principal the hub writes owner-approved facts
// as.
var FactSheetPrincipal = Principal{Kind: PrincipalSystem, ID: "facts"}

// FactSheetWriter reports whether p may write a project's fact sheet: the
// owner, or the hub applying a fact the owner approved. Every line on it is
// presented to agents as the owner's settled word, so a session proposes
// facts (an owner ask) instead of writing them. The projects store enforces
// it on the resolved slug of every write path; launch context ignores a
// sheet whose head revision another principal wrote.
func FactSheetWriter(p Principal) bool {
	if p.OnBehalfOf != nil {
		return p.OnBehalfOf.Kind == PrincipalOwner
	}
	return p.Kind == PrincipalOwner || (p.Kind == FactSheetPrincipal.Kind && p.ID == FactSheetPrincipal.ID)
}

// FactSheetForbiddenText is the refusal text for other writers.
const FactSheetForbiddenText = "only the owner writes the fact sheet; propose a fact with propose_fact (`pyre propose fact`), which asks the owner"
