-- 0005 · Las denegaciones y los replays también son invocaciones (INT-014).
--
-- `INT-013` cerró el registro de lo que se EJECUTA, por las tres puertas y por construcción: el
-- registro vive dentro de `Execute`, que es el único camino para correr un tool. Esta migración
-- cubre los dos hechos que, por diseño, no pasan por ahí:
--
--   · UNA DENEGACIÓN no llega a `Execute` —la comprobación de política es estrictamente anterior—,
--     y con run se journaliza desde `INT-011`, pero sin run no se registraba en ninguna parte. Todo
--     llamador MCP externo está en ese caso.
--   · UN REPLAY servido desde la caché de `tool_executions` responde `deduplicated: true` sin
--     ejecutar, así que tampoco pasa por `Execute`. El llamador llamó y le respondimos: ocurrió.
--
-- POR QUÉ NO SE UNIFICÓ EN UN SOLO PUNTO DE PASO, que era la opción elegante y se rechazó a
-- propósito: el único chokepoint que cubriría las denegaciones sería hacer que `Execute` reciba la
-- decisión y se niegue a ejecutar cuando sea un rechazo. Eso mete en el ejecutor una rama «¿debo
-- ejecutar?» gobernada por un flag que le pasan, y hoy es IMPOSIBLE que el ejecutor corra un tool
-- denegado porque nunca ve uno (ADR-0001, y el test de SEC-001 depende de esa separación). Una fila
-- de auditoría que falta es barata; un segundo lugar capaz de ejecutar un denegado es la peor clase
-- de fallo disponible. Así que se registra en los tres sitios de decisión con un helper que SOLO
-- registra —sin rama de ejecución— y un guard en `make lint` que falla si una puerta sabe ejecutar
-- y no sabe registrar un rechazo. Es «olvidarse se detecta» en vez de «olvidarse es imposible», y
-- se dice así en vez de presumir de lo segundo.
ALTER TABLE tool_invocations DROP CONSTRAINT IF EXISTS tool_invocations_outcome_valid;
ALTER TABLE tool_invocations ADD CONSTRAINT tool_invocations_outcome_valid
    CHECK (outcome IN ('ok', 'error', 'denied', 'replayed'));

-- La disposición de INT-010, no un booleano: «rechazado» y «rechazado pero una persona podría
-- aprobarlo» son cosas distintas, y una auditoría que las confunde no puede responder por qué un
-- run se detuvo. Cadena vacía para las filas que no son denegaciones.
ALTER TABLE tool_invocations ADD COLUMN IF NOT EXISTS disposition TEXT NOT NULL DEFAULT '';
-- Qué política decidió. Sin esto, «fue denegado» no es accionable: nadie sabe qué cambiar.
ALTER TABLE tool_invocations ADD COLUMN IF NOT EXISTS policy_id TEXT NOT NULL DEFAULT '';

-- duration_ms ya era NULLABLE y ahora ese NULL significa algo preciso y se usa: nada se ejecutó.
-- Una denegación y un replay no tienen duración de ejecución, y escribir 0 diría «corrió y tardó
-- nada», que es el error que `OBS-008` corrigió para el costo.
COMMENT ON COLUMN tool_invocations.duration_ms IS
    'NULL = nada se ejecutó (denegación o replay). 0 = ejecutó y tardó menos de un milisegundo.';

-- La disposición solo tiene sentido en una denegación, y una denegación sin disposición no explica
-- nada. La base lo exige en vez de confiar en que las tres puertas acierten.
ALTER TABLE tool_invocations DROP CONSTRAINT IF EXISTS tool_invocations_disposition_iff_denied;
ALTER TABLE tool_invocations ADD CONSTRAINT tool_invocations_disposition_iff_denied
    CHECK (
        (outcome = 'denied' AND disposition <> '') OR
        (outcome <> 'denied' AND disposition = '')
    );
