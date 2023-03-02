module github.com/nicois/pytestw

go 1.18

replace github.com/nicois/git => ../git

replace github.com/nicois/file => ../file

replace github.com/nicois/pyast => ../pyast

replace github.com/nicois/cache => ../cache

require (
	github.com/frioux/shellquote v0.0.2
	github.com/fsnotify/fsnotify v1.6.0
	github.com/nicois/cache v0.0.0-20230227103345-85fe1201b3d4
	github.com/nicois/file v0.0.0-20230227095154-1d4e7ac1358a
	github.com/nicois/git v0.0.0-20230226061620-cf42fa66944f
	github.com/nicois/pyast v0.0.0-00010101000000-000000000000
	github.com/sirupsen/logrus v1.9.0
	golang.org/x/sys v0.5.0
)

require golang.org/x/sync v0.1.0 // indirect
