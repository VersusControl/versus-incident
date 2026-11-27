-- Range indexes that also match the scope, root or namespace by equality
-- need the btree operator classes for GiST. The extension ships with
-- PostgreSQL and is trusted, so a role with CREATE on the database adds it.
CREATE EXTENSION IF NOT EXISTS btree_gist;
