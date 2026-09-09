# Contratos compartidos — copia vendorizada

Estos ficheros **no son de Aeon**: son el contrato acordado entre Synaptum, Aeon y Axonium, y su
copia canónica vive en la carpeta de coordinación de los tres equipos
(`coordinacion_project/contratos/`), junto al `cordinacion.md` donde se anuncian los cambios.

Aquí hay una copia porque el CI de Aeon tiene que poder ejecutar los fixtures sin salir del repo.
Cada uno de los tres proyectos hace lo mismo con la suya.

**La duplicación es real y conviene no disimularla.** El handle para detectar que dos copias han
divergido es el campo `version` de cada fichero de fixtures: si la copia canónica sube de versión y
esta no, esta está vieja. No hay nada automático que lo compruebe todavía — moverlo a un repo con CI
propio es justamente lo que resolvería eso, y se decidió no hacerlo por ahora.

Quien cambie el contrato lo cambia en la copia canónica primero y lo anuncia en `cordinacion.md`.
