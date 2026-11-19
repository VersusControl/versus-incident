-- versus:nontransactional
DROP INDEX CONCURRENTLY IF EXISTS kubernetes_evidence_issue_latest;
DROP INDEX CONCURRENTLY IF EXISTS kubernetes_evidence_issue_namespace;
DROP INDEX CONCURRENTLY IF EXISTS kubernetes_evidence_issue_root;
DROP INDEX CONCURRENTLY IF EXISTS kubernetes_evidence_issue_time;
