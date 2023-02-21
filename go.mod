module github.com/nicois/pytestw

go 1.18

// replace github.com/nicois/git => ../git

// replace github.com/nicois/file => ../file

// replace github.com/nicois/pyast => ../pyast

// replace github.com/nicois/cache => ../cache

require (
	github.com/frioux/shellquote v0.0.2
	github.com/fsnotify/fsnotify v1.6.0
	github.com/getsentry/sentry-go v0.21.0
	github.com/nicois/cache v0.0.0-20230523010523-8dd287f76d21
	github.com/nicois/file v0.0.0-20230309073744-e6bf63959c2a
	github.com/nicois/git v0.0.0-20230228004916-5e651dd241ec
	github.com/nicois/pyast v0.0.0-20230531023401-9ec6ef8e9503
	github.com/sirupsen/logrus v1.9.0
	golang.org/x/sys v0.6.0
)

require (
	github.com/alecthomas/participle/v2 v2.0.0 // indirect
	golang.org/x/sync v0.1.0 // indirect
	golang.org/x/text v0.8.0 // indirect
)
