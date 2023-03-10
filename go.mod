module github.com/nicois/pytestw

go 1.18

replace github.com/nicois/git => ../git

replace github.com/nicois/file => ../file

replace github.com/nicois/pyast => ../pyast

replace github.com/nicois/cache => ../cache

require (
	github.com/frioux/shellquote v0.0.2
	github.com/fsnotify/fsnotify v1.6.0
	github.com/nicois/cache v0.0.0-20230309075418-3aae7a3eee00
	github.com/nicois/file v0.0.0-20230309073744-e6bf63959c2a
	github.com/nicois/git v0.0.0-20230228004916-5e651dd241ec
	github.com/nicois/pyast v0.0.0-00010101000000-000000000000
	github.com/sirupsen/logrus v1.9.0
	golang.org/x/sys v0.5.0
)

require golang.org/x/sync v0.1.0 // indirect
