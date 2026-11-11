-- A saving decision names its saving by an ID derived from the saving's
-- current shape, so the ID alone cannot say what was decided once the saving
-- is no longer produced. These columns keep what the decision referred to,
-- captured when it was made: the payer and account, the saving kind and the
-- title as it was shown. orphaned_at is set while the saving is not produced
-- and cleared when it returns. A payer delete removes its decisions by payer;
-- retention removes decisions orphaned for long enough by orphaned_at.

ALTER TABLE finops_saving_decisions ADD COLUMN IF NOT EXISTS payer TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE finops_saving_decisions ADD COLUMN IF NOT EXISTS account TEXT COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE finops_saving_decisions ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT '';
ALTER TABLE finops_saving_decisions ADD COLUMN IF NOT EXISTS title TEXT NOT NULL DEFAULT '';
ALTER TABLE finops_saving_decisions ADD COLUMN IF NOT EXISTS orphaned_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS finops_saving_decisions_payer ON finops_saving_decisions (org_id, payer);
CREATE INDEX IF NOT EXISTS finops_saving_decisions_orphaned ON finops_saving_decisions (org_id, orphaned_at);
