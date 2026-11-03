package snapshot

// The tests open snapshots with database/sql to look inside them and to
// build C#-style ones by hand. They use modernc's SQLite, independent of the
// plugin that the package itself uses, so that the two implementations check
// each other. It is not linked into the program.
import _ "modernc.org/sqlite"
