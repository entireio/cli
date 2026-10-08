package cli

// statusCompleted is the "completed" status string shared by CI check runs
// (trail mergeability and merge) and task records in `checkpoint list
// --pending` JSON. One constant keeps the literal from drifting between them.
const statusCompleted = "completed"
