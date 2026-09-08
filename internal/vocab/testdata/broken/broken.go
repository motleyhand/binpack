// Package broken does not type-check, which is the point.
package broken

const CodeOne = "one"

// Undeclared on purpose: a reader that answers anyway is answering with
// whatever it happened to resolve.
const CodeTwo = undeclared + "two"
