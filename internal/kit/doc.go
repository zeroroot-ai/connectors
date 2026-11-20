// Package kit holds the parts that every connector server shares: the
// configuration from the environment, the secret type, the logger, the HTTP
// client for the vendor API, the input validation helpers, the tool helpers
// and the streamable HTTP server.
//
// A server reads each credential and each base URL from its environment. No
// function in this package writes a credential to a log or to an error.
package kit
