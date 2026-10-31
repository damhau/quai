module github.com/damhau/quai/plugins/template

go 1.24

require github.com/damhau/quai/plugins/sdk v0.0.0

// The SDK next to this template, in the Quai repository. A plugin of its own repository requires
// a released version instead.
replace github.com/damhau/quai/plugins/sdk => ../sdk
