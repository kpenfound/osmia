package checkselect

// The request wording is shared by the service and smartcheck.
const (
	process       = "Osmia is choosing which of a project's Dagger checks to run on a change, in three stages. Stage 1, breadth: for each top-level check, could the change affect it or any of its items at all? A no skips the check and everything in it. Stage 2, depth: for each collection kept by stage 1, at which level of its hierarchy should stage 3 judge it? Stage 3, ranking: for each remaining link, could the change alter its result? A yes runs it. Missing an affected check can let a regression through; running an unaffected one only costs time."
	breadthStage  = "Stage 1 of 3, breadth. Could the change in the state alter the result of the Dagger check `%s`, or of any of its items?"
	breadthYes    = "The check, or at least one of its items, builds or exercises a changed file, or code, configuration or generated files the change affects. It goes on to stage 2 or 3 for a finer judgment."
	breadthNo     = "Nothing the check or any of its items depends on is touched by the change. The check and every item in it are skipped with no further judgment, so answer no only when the change clearly cannot reach it."
	depthStage    = "Stage 2 of 3, depth. Stage 1 judged that the change could affect the collection `%s`. Choose the level at which stage 3 should judge its links."
	depthGuidance = "The levels nest: each item contains every item at the levels below it. Choosing a level never drops a check. It decides how finely stage 3 divides this collection, and each link at the chosen level runs everything beneath it. A level that is too broad cannot exclude anything: nearly every link holds code the change touches, so nearly the whole collection runs. A level that is too fine asks stage 3 about items the change does not identify: an individual test's name rarely says what code it exercises, so stage 3 has to guess, and a wrong guess skips an affected test. Choose the level whose items correspond most directly to the files and directories in the change, so each link can be judged from the change itself. For Go collections that is usually `go-package`: a package is a directory that the changed files name, and imports connect it to other packages. `go-module` is too broad and `go-test` too fine. Choose a finer level only when the change itself identifies items at that level. Choose a coarser one only when the change cannot tell items at this level apart."
	depthOption   = "Judge each of the `%d` `%s` links, such as `%s`."
	depthContains = " Each runs every `%s` item in it."
	broadReason   = "covers the whole collection; already kept by stage 1"
	rankingStage  = "Stage 3 of 3, ranking. Earlier stages kept this check and chose how finely to divide its collection. Could the change in the state alter the result of the Dagger check `%s`?"
	rankingYes    = "The check exercises a changed file, or code, configuration or generated files the change affects. It runs."
	rankingNo     = "Nothing the check depends on is touched by the change. It is skipped."
)
