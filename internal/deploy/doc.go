// Package deploy owns models and deployments: the models and deployments
// tables, the node listing used for manual placement, and the loop that
// walks each deployment through Oakestra until its engine answers.
//
// It is deliberately not a reconciler. Every deployment row carries its
// own state, and each tick of the loop runs one idempotent step per row:
//
//	pending     ensure the application, adopt or create the service, deploy
//	scheduling  wait for Oakestra to report the instance RUNNING
//	starting    wait for the engine to answer /health and /v1/models
//	ready       probe; three failures in a row make it unhealthy
//	unhealthy   probe; the first success makes it ready again
//	failed      untouched until Retry or Delete
//	deleting    delete the service, then the row
//
// A failed deployment is never retried on its own and nothing is placed
// automatically. A transient Oakestra error leaves the row in its state with
// the error text recorded; the next tick tries again.
//
// Only ready rows are routable. Manager.OnChange tells the gateway about the
// routable set; deploy never imports the gateway.
package deploy
