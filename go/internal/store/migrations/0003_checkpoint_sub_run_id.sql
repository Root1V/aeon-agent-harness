-- 0003 · A checkpoint's identity includes the sub-run it belongs to (VRT-AEON-004).
--
-- WHY, in Veritium's words: "que un nieto de una delegación perdiera registros sin error sería justo
-- el tipo de fallo que no detectaríamos". With nested delegation, two different sub-runs of the same
-- run can legitimately use the same step_id — they are separate loops, each numbering its own steps.
-- The primary key did not include the sub-run, so the second one's `attempted`/`completed` entry was
-- a DUPLICATE of the first's: Append returned Duplicate=true, wrote nothing, and reported success.
-- The journal then said a step had already run when it had not, and a resume would skip real work.
--
-- THE SENTINEL IS IN THE SCHEMA, NOT IN THE CLIENT, which is the one thing Veritium asked for beyond
-- the key itself: "el centinela lo decide quien guarda los datos; solo pedimos que quede escrito en
-- el esquema y que el cliente no tenga que elegir". `NOT NULL DEFAULT ''` does exactly that — a
-- caller with no sub-run omits the field and the row still has a definite value, so the key is total
-- and no client ever has to invent a placeholder. A nullable column would have been worse than a
-- choice: NULL is not equal to NULL in a unique index, so every root-run entry would have been
-- unique against itself and deduplication would have stopped working altogether.
--
-- '' IS THE ROOT RUN and sub_run_id is an OPAQUE PATH (agreed by both sides): nothing here parses
-- it, splits it or reads depth out of it. A grandchild is just a longer string.
--
-- DESTRUCTIVE, like the six in 0002 and for the same reason: it replaces a PRIMARY KEY, which no
-- CREATE TABLE IF NOT EXISTS can express. Existing rows take '' and keep their identity, so a
-- journal written before this migration reads back exactly as it did.
ALTER TABLE run_checkpoints ADD COLUMN IF NOT EXISTS sub_run_id TEXT NOT NULL DEFAULT '';

ALTER TABLE run_checkpoints DROP CONSTRAINT IF EXISTS run_checkpoints_pkey;
ALTER TABLE run_checkpoints ADD PRIMARY KEY (tenant_id, run_id, sub_run_id, step_id, phase);

-- seq stays RUN-WIDE and is deliberately not part of the key change: NextSeq answers "where does
-- this run's journal continue", and one sequence per run keeps a delegation's entries ordered
-- against its parent's. A per-sub-run sequence would have made two entries with the same seq.
