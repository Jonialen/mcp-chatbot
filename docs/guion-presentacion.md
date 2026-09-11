# Guion de presentación

Proyecto 1, CC3067 Redes. Uso de un protocolo existente: Model Context Protocol.

Cubre las tres secciones que pide el enunciado: características implementadas,
dificultades y cómo se resolvieron, y lecciones aprendidas.

---

## Cifras del proyecto

| | |
| --- | --- |
| Código propio | 7,149 líneas de Go |
| Pruebas | 4,956 líneas, 112 funciones de prueba |
| Paquetes | 17 (12 en el anfitrión, 5 en BrewOps) |
| Servidores conectados a la vez | 4 |
| Herramientas expuestas al modelo | 38 |
| Lenguajes involucrados | 3 (Go, TypeScript, Python) |
| Transportes implementados | 2 (stdio y Streamable HTTP) |
| SDK de MCP usados | ninguno |

---

## Características implementadas

### La idea en una frase

Un chatbot de consola que actúa como anfitrión MCP: lanza varios servidores
del protocolo, junta todas sus herramientas en una sola lista, se la entrega a un
modelo, y ejecuta lo que el modelo pide.

### Demostración sugerida, en este orden

1. El protocolo, crudo. Arrancar el anfitrión y mostrar el registro de frames
en pantalla. Lo primero que se ve es el handshake completo:

```
--> request initialize    {"jsonrpc":"2.0","id":1,"method":"initialize",...}
<-- result id=1           {"result":{"protocolVersion":"2025-06-18",...}}
--> notify notifications/initialized
```

Vale detenerse un segundo: son tres mensajes, no dos. El tercero no es una
cortesía; un servidor puede rechazar peticiones hasta recibirlo.

2. Seis servidores, tres lenguajes, cinco autores, dos transportes.

```
brewops     stdio  Go          propio       9 tools
filesystem  stdio  TypeScript  Anthropic   14 tools
git         stdio  Python      Anthropic   12 tools
hotel       stdio  Python      compañero    9 tools
rrhh        stdio  Python      compañero    6 tools
netprobe    http   Go/Docker   propio       3 tools
                                           53 tools
```

El punto que hay que hacer explícito: el anfitrión no adapta nada para
ninguno. Lanza un proceso, le habla JSON-RPC, y nunca se entera de en qué
lenguaje está escrito del otro lado.

3. El escenario que pide el enunciado. Pedirle que cree un README, lo agregue
a git y haga commit. Seis llamadas a herramientas coordinadas por el modelo, a
través de dos servidores distintos:

```
⚙ filesystem__list_allowed_directories   ✓
⚙ git__git_status                        ✓
⚙ filesystem__write_file                 ✓
⚙ git__git_add                           ✓
⚙ git__git_commit                        ✓
⚙ git__git_log                           ✓
```

Detalle que vale mencionar: consultó los directorios permitidos antes de
escribir. Nadie se lo programó; el modelo usó una herramienta para orientarse.

4. El servidor propio. Pedirle la receta para 350 g de Ethiopia Guji en V60.
Devuelve 21.0 g a 1:16.7, con horario de vertidos y estado de reposo del grano.

El argumento: esos números no los inventó el modelo, los calculó el servidor.
Un modelo produce cifras que parecen correctas; una cafetería que necesita la
misma taza dos veces no puede trabajar con cifras que parecen correctas.

5. El servidor remoto en contenedor. Preguntarle qué TLS negocia un sitio.
Llama dos herramientas en paralelo y responde con TLS 1.3, el cifrado y el
estado del puerto 443.

### Los dos extras

JSON-RPC directo, sin SDK de MCP. Todo lo que aparece en el registro de
frames fue construido y parseado por código propio: el envoltorio de mensajes, la
correlación de identificadores, ambos transportes, y también el lado servidor.

Interfaz de terminal. Construida con Bubble Tea: selector de servidores antes de
conectar, registro de frames en vivo con resúmenes de una línea, progreso de las
herramientas mientras se ejecutan, y comandos `/tools`, `/log`, `/usage`,
`/reset`. Toda salida de modelo y de servidor se trata como texto, nunca como
secuencias de control de terminal, porque un servidor de un tercero no debería
poder repintar la pantalla del anfitrión.

---

## Dificultades y cómo se resolvieron

Estas son reales; todas costaron tiempo y ninguna estaba en la documentación como
advertencia.

### El servidor que se colgaba sin motivo aparente

Síntoma. La sesión se quedaba esperando una respuesta que nunca llegaba.
Parecía un interbloqueo del protocolo.

Causa. Un servidor MCP escribe sus diagnósticos a `stderr`. Si nadie lee ese
pipe, el búfer del sistema operativo se llena, el servidor se bloquea en su
siguiente escritura, y deja de responder.

Solución. Una goroutine dedicada exclusivamente a drenar `stderr`.

Por qué importa. No es un problema del protocolo, es control de flujo de la
capa que sustituye al transporte cuando la comunicación es entre procesos.

### El campo que desaparecía

Síntoma. El servidor oficial de filesystem rechazaba las llamadas a
herramientas sin argumentos con `-32602: expected object, received undefined`.

Causa. El campo `arguments` estaba marcado `omitempty`, así que con un mapa
vacío desaparecía del mensaje. La especificación dice que es opcional; los
servidores que validan contra JSON Schema `type: object` dicen que no.

Solución. Enviar siempre `"arguments": {}`.

Lección. Sé permisivo en lo que aceptás, estricto en lo que emitís.

### La firma que había que devolver

Síntoma. El primer turno funcionaba, el segundo fallaba siempre:
`Function call is missing a thought_signature in functionCall parts`.

Causa. Los modelos Gemini 3 firman cada llamada a herramienta y exigen esa
firma de vuelta al continuar la conversación.

Solución. Y acá está lo interesante: la abstracción se filtró. El puerto
definía una llamada como `{id, nombre, argumentos}`, reconstruible desde datos,
pero con firma no se puede reconstruir, solo transportar.

La solución no fue exponer el tipo del proveedor, sino agregar un campo de bytes
opacos que el anfitrión transporta sin mirar:

```go
// Datos opacos que el proveedor adjuntó y espera de vuelta sin modificar.
ProviderState []byte
```

Lección. Cuando una abstracción se filtra, el arreglo es un paso opaco, no
exponer el tipo del vendedor.

### Dos cuotas distintas disfrazadas del mismo error

Síntoma. Error 429 en medio de una demostración.

Causa. El nivel gratuito tiene dos límites: 20 peticiones por día y 5 por
minuto, y ambos llegan como 429.

Solución. Distinguirlos, porque necesitan respuestas opuestas:

| Límite | Qué hacer |
| --- | --- |
| Por minuto | Esperar. Se cura solo. |
| Por día | Esperar es lo único que no puede funcionar. Cambiar de modelo. |

Además, el servicio informa cuánto esperar (`retryDelay: 53s`) y el cliente
original lo ignoraba, agotando los reintentos antes de que la ventana rotara.

Lección. Un limitador de tasa sabe cuándo rota su ventana. Esa
información es mejor que cualquier curva de espera calculada.

### El contenedor que construía bien y fallaba al arrancar

Dos errores del mismo tipo: no se manifiestan donde se originan.

| Decisión equivocada | Qué pasa |
| --- | --- |
| Compilar sin `CGO_ENABLED=0` | El build pasa; el contenedor falla al arrancar |
| Usar `scratch` como imagen base | Todo funciona salvo TLS, con un error que aparenta ser de red |

El segundo es el peor: sin certificados raíz, toda conexión HTTPS falla con
"autoridad no verificable", y se pierde tiempo buscando el problema en la red.

### El límite invisible del lector

`bufio.Scanner` rechaza líneas de más de 64 KiB por omisión. Una respuesta de
`tools/list` real ocupa 13 KB, y leer un archivo mediano la supera. La solución
fue `bufio.Reader.ReadBytes`, que crece sin techo fijo.

---

## Lecciones aprendidas

### Las pruebas unitarias solo demuestran lo que uno envía

Solo el servicio demuestra lo que acepta. Los tres hallazgos que costaron más
tiempo (la firma de las llamadas, la latencia del razonamiento, los modelos
retirados) eran invisibles hasta ejecutar contra la API real. Por eso las
pruebas de integración quedaron en el repositorio, protegidas por una variable de
entorno para que la suite siga corriendo sin credenciales.

### Para SDKs tipados, el código instalado le gana a la documentación

La documentación de Gemini decía "solo se soporta un subconjunto del esquema
OpenAPI" y no enumeraba cuál. La respuesta salió de `go doc` contra el
paquete ya instalado, que reveló un campo que acepta JSON Schema directamente y
volvió innecesario el traductor que se había planificado.

La documentación dice lo que alguien escribió; el tipo dice lo que el compilador
acepta.

### El error de una herramienta pertenece al modelo

La decisión de diseño más importante del protocolo es también la más fácil de
implementar mal. Una herramienta que se ejecutó y falló no viaja como error
de JSON-RPC: viaja como respuesta exitosa con `isError`.

El destinatario de ese fallo es el modelo, que puede leerlo y elegir otro camino.
Tratarlo como excepción termina la conversación por algo recuperable.

### Un análisis por capas puede salir vacío, y eso enseña

Cinco de los seis servidores no usan la pila de red en absoluto:

```
fd 0 -> pipe:[580772]     sockets: 0     conexiones: 0
fd 1 -> pipe:[580774]
```

Sin trama, sin paquete IP, sin puerto TCP. El valor está en el contraste con el
servidor remoto: muestra qué trabajo desaparece y quién lo hace en su lugar
cuando no hay red de por medio.

### Elegir tecnología por lo que mide la rúbrica

Se evaluó Rust y se descartó: no tiene SDK oficial del proveedor de modelos, lo
que habría obligado a implementar dos protocolos a mano en vez de uno. El
segundo no sumaba nada evaluable.

Dificultad técnica que no se califica es tiempo robado a lo que sí se califica.

### Implementar el protocolo enseña lo que el SDK esconde

Escribir JSON-RPC a mano obligó a resolver la correlación de identificadores, la
distinción entre fallo de protocolo y fallo de herramienta, el drenaje de
`stderr`, la negociación de versiones. Un SDK habría resuelto los cuatro en
silencio, y con ellos el aprendizaje.

---

## Cierre

El enunciado abre señalando que cada empresa define su propia forma de integrar
herramientas, y que por eso no existe interoperabilidad.

Este proyecto conecta seis servidores, escritos en tres lenguajes, por cinco
autores distintos, sobre dos transportes distintos, y le entrega sus 53
herramientas a un modelo de Google. No se escribió una línea de adaptación para
ninguno.

Que los servidores oficiales de Anthropic funcionen con un modelo que no es de
Anthropic es la demostración más directa del argumento que MCP hace sobre sí
mismo.

---

## Anexo: preguntas probables

¿Por qué no usaste el SDK oficial de MCP?
Por el punto extra, y porque implementarlo obliga a entender la correlación de
identificadores, la delimitación de mensajes y el modelo de errores. El SDK los
resuelve en silencio.

¿Por qué Google y no Anthropic?
El enunciado pide "un LLM" y sugiere Anthropic por sus créditos gratuitos, no por
una razón técnica. Usar un modelo que no es de Anthropic con servidores que sí lo
son demuestra la independencia entre herramienta y modelo, que es la tesis del
protocolo.

¿Qué pasa si un servidor de un compañero falla?
Se reporta y se continúa sin él. El anfitrión nunca deja de arrancar por un
servidor caído: con varios conectados, un vecino roto no puede costar las
herramientas de los demás.

¿Cómo evitás que dos servidores choquen de nombres?
Cada herramienta se publica con el prefijo de su servidor
(`filesystem__read_file`). Si el nombre calificado excede el límite del
proveedor, se recorta el prefijo, nunca el nombre de la herramienta, que es
lo que el modelo lee para decidir.

¿El servidor remoto es seguro?
Hace peticiones de red por cuenta de quien lo llame, así que rechaza destinos
internos: loopback, rangos privados y enlace local, incluido `169.254.169.254`,
el servicio de metadatos de la nube. El contenedor corre sin privilegios, sin
capacidades y con sistema de archivos de solo lectura.
