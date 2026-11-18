// Package address holds the addresses of Yonder's own servers that are
// built into the app. There are three parts, and each can be on a server of
// its own: the website, the web app, and the service the apps call. (The
// relay's name is tunnel.Relay.) An app that is installed keeps calling the
// names it was built with, so they are changed with care.
package address

const (
	// Website is Yonder's website: pages only. The app only opens links
	// to it (the donate page, the download page), and works without it.
	Website = "https://getyonder.org/"

	// WebApp is Yonder in a browser. A pairing link is this address with
	// the pairing address after a "#".
	WebApp = "https://app.getyonder.org/"

	// Service is the host name of the service the apps call: the meeting
	// point for nearby devices and rooms, the list of relays and the
	// update check.
	Service = "api.getyonder.org"
)
