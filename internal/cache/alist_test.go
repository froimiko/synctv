package cache

import "testing"

func TestResolveAlistPath(t *testing.T) {
	tests := []struct {
		name     string
		basePath string
		subPath  string
		want     string
		wantErr  bool
	}{
		{name: "empty", basePath: "/movies", subPath: "", want: "/movies"},
		{name: "child", basePath: "/movies", subPath: "child", want: "/movies/child"},
		{name: "grandchild", basePath: "/movies", subPath: "child/grandchild", want: "/movies/child/grandchild"},
		{name: "dot child", basePath: "/movies", subPath: "./child", want: "/movies/child"},
		{name: "duplicate slash", basePath: "/movies", subPath: "child//grandchild", want: "/movies/child/grandchild"},
		{name: "private sibling escape", basePath: "/movies", subPath: "../movies-private", wantErr: true},
		{name: "root escape", basePath: "/movies", subPath: "../../root", wantErr: true},
		{name: "same prefix sibling", basePath: "/movies", subPath: "../movies2", wantErr: true},
		{name: "root descendant", basePath: "/", subPath: "movies/child", want: "/movies/child"},
		{name: "relative base", basePath: "movies", subPath: "child", want: "/movies/child"},
		{name: "base parent segment", basePath: "/srv/../movies", subPath: "child", wantErr: true},
		{name: "subpath parent segment", basePath: "/movies", subPath: "series/../secret", wantErr: true},
		{name: "base backslash", basePath: "\\movies", subPath: "child", wantErr: true},
		{name: "subpath backslash", basePath: "/movies", subPath: "series\\episode", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveAlistPath(tt.basePath, tt.subPath)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error state mismatch")
			}
			if err == nil && got != tt.want {
				t.Fatalf("path = %q, want %q", got, tt.want)
			}
			if err != nil && err.Error() != "sub path is not in parent path" {
				t.Fatalf("unexpected error message")
			}
		})
	}
}

func TestResolveAlistSearchSubPath(t *testing.T) {
	tests := []struct {
		name         string
		sharedRoot   string
		searchRoot   string
		resultParent string
		fileName     string
		want         string
		wantErr      bool
	}{
		{name: "root file", sharedRoot: "/movies", searchRoot: "/movies", resultParent: "/movies", fileName: "episode.mkv", want: "/episode.mkv"},
		{name: "nested file", sharedRoot: "/movies", searchRoot: "/movies", resultParent: "/movies/series", fileName: "episode.mkv", want: "/series/episode.mkv"},
		{name: "nested search root", sharedRoot: "/movies", searchRoot: "/movies/public", resultParent: "/movies/public", fileName: "episode.mkv", want: "/public/episode.mkv"},
		{name: "nested search descendant", sharedRoot: "/movies", searchRoot: "/movies/public", resultParent: "/movies/public/series", fileName: "episode.mkv", want: "/public/series/episode.mkv"},
		{name: "outside nested search root", sharedRoot: "/movies", searchRoot: "/movies/public", resultParent: "/movies/private", fileName: "secret.mkv", wantErr: true},
		{name: "same prefix nested search sibling", sharedRoot: "/movies", searchRoot: "/movies/public", resultParent: "/movies-public", fileName: "secret.mkv", wantErr: true},
		{name: "shared filesystem root", sharedRoot: "/", searchRoot: "/", resultParent: "/series", fileName: "episode.mkv", want: "/series/episode.mkv"},
		{name: "root shared but outside search", sharedRoot: "/", searchRoot: "/public", resultParent: "/private", fileName: "secret.mkv", wantErr: true},
		{name: "search root outside shared root", sharedRoot: "/movies", searchRoot: "/private", resultParent: "/private", fileName: "secret.mkv", wantErr: true},
		{name: "same prefix sibling", sharedRoot: "/movies", searchRoot: "/movies", resultParent: "/movies-private", fileName: "secret.mkv", wantErr: true},
		{name: "parent traversal", sharedRoot: "/movies", searchRoot: "/movies", resultParent: "/movies/../movies-private", fileName: "secret.mkv", wantErr: true},
		{name: "shared root parent traversal", sharedRoot: "/srv/../movies", searchRoot: "/movies", resultParent: "/movies", fileName: "episode.mkv", wantErr: true},
		{name: "search root parent traversal", sharedRoot: "/movies", searchRoot: "/movies/../private", resultParent: "/private", fileName: "secret.mkv", wantErr: true},
		{name: "shared root backslash", sharedRoot: "\\movies", searchRoot: "/movies", resultParent: "/movies", fileName: "secret.mkv", wantErr: true},
		{name: "search root backslash", sharedRoot: "/movies", searchRoot: "/movies\\public", resultParent: "/movies/public", fileName: "secret.mkv", wantErr: true},
		{name: "result parent backslash", sharedRoot: "/movies", searchRoot: "/movies", resultParent: "/movies\\..\\private", fileName: "secret.mkv", wantErr: true},
		{name: "normalized paths", sharedRoot: "//movies/./", searchRoot: "//movies//public/.", resultParent: "//movies//public/series/./season-1", fileName: "episode.mkv", want: "/public/series/season-1/episode.mkv"},
		{name: "empty name", sharedRoot: "/movies", searchRoot: "/movies", resultParent: "/movies", fileName: "", wantErr: true},
		{name: "dot name", sharedRoot: "/movies", searchRoot: "/movies", resultParent: "/movies", fileName: ".", wantErr: true},
		{name: "parent name", sharedRoot: "/movies", searchRoot: "/movies", resultParent: "/movies", fileName: "..", wantErr: true},
		{name: "nested name", sharedRoot: "/movies", searchRoot: "/movies", resultParent: "/movies", fileName: "series/episode.mkv", wantErr: true},
		{name: "backslash parent name", sharedRoot: "/movies", searchRoot: "/movies", resultParent: "/movies", fileName: "..\\secret", wantErr: true},
		{name: "backslash nested name", sharedRoot: "/movies", searchRoot: "/movies", resultParent: "/movies", fileName: "series\\episode.mkv", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveAlistSearchSubPath(tt.sharedRoot, tt.searchRoot, tt.resultParent, tt.fileName)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error state mismatch: %v", err)
			}
			if err == nil && got != tt.want {
				t.Fatalf("sub path = %q, want %q", got, tt.want)
			}
		})
	}
}
