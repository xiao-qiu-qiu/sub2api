-- Confirmed quality degradation shares the existing durable alert queue.
ALTER TABLE account_ops_alerts DROP CONSTRAINT account_ops_alerts_kind_check;
ALTER TABLE account_ops_alerts ADD CONSTRAINT account_ops_alerts_kind_check
 CHECK(kind IN ('balance_low','weekly_quota','balance_threshold','quota_threshold','quality_degraded'));
