-- 0006 · El gasto que no deja rastro porque la petición «no salió bien» (FIN-003).
--
-- `Gateway.Decide` descarta un candidato de dos maneras: el proveedor devuelve error, o la respuesta
-- es INUTILIZABLE (`emptyAnswer`, `MDL-015`: un 200 sin contenido y sin tool calls). En el segundo
-- caso el modelo SÍ corrió: hubo tokens de entrada y de salida, reportados en la respuesta que
-- tiramos. Hasta esta migración la fachada contestaba con la respuesta del candidato siguiente —o un
-- 502 si no había— y el intento descartado no dejaba fila: tokens pagados que no ve el dashboard, ni
-- `SpendForRun`, ni por tanto el techo de presupuesto.
--
-- LA DIRECCIÓN DEL ERROR ERA LA MALA, y es lo que lo hace un defecto y no una imprecisión: un techo
-- que cuenta de menos no avisa cuando debería. Un techo que cuenta de más molesta. Y lo señalaron dos
-- equipos la misma semana por caminos distintos — Argus, porque su agente de FinOps va a leer esta
-- tabla como la verdad del coste, y Prometheus con su cliente desconectado: «un pico de 5xx no es la
-- única forma de desperdiciar cómputo».
--
-- POR QUÉ UNA COLUMNA Y NO UNA FILA IGUAL A LAS DEMÁS: un intento descartado no es una llamada
-- servida. Sumarlo a `call_count` haría que «llamadas» significara «intentos», y quien divida coste
-- entre llamadas obtendría una media de algo que no ocurrió. Así que las columnas que ya existían
-- siguen contando lo SERVIDO y lo descartado se cuenta aparte — el mismo trato que `OBS-008` le dio
-- a las llamadas sin precio: nunca ocultas, nunca sumadas en silencio.
--
-- NULL ES «SERVIDA», que es el valor de la inmensa mayoría de las filas y el que toman todas las
-- existentes. Texto y no booleano por el mismo motivo que `disposition` en `0005`: «descartada
-- porque el proveedor falló» y «descartada porque la respuesta no servía» son hechos distintos, y una
-- auditoría que los confunde no puede responder por qué un run pagó dos veces.
ALTER TABLE model_gateway_costs ADD COLUMN IF NOT EXISTS discarded_reason TEXT;

-- El índice es por (tenant, run) y parcial: todas las agregaciones filtran por él para no contar un
-- intento como una llamada, y las filas descartadas son una minoría del total.
CREATE INDEX IF NOT EXISTS model_gateway_costs_discarded_idx
    ON model_gateway_costs (tenant_id, run_id) WHERE discarded_reason IS NOT NULL;
