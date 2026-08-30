# Análisis por capas y conclusiones

Proyecto 1 — CC3067 Redes, Universidad del Valle de Guatemala.

Este documento explica qué ocurre en las capas de enlace, red, transporte y
aplicación cuando el anfitrión se comunica con un servidor MCP, y cierra con las
conclusiones del proyecto.

Todos los números y descriptores que aparecen aquí fueron medidos sobre el
sistema en ejecución, no estimados.

---

## 1. El proyecto tiene dos transportes, y dan respuestas distintas

MCP define dos transportes, y ambos están implementados en este proyecto:

| Transporte | Servidores | Dónde corre el servidor |
| --- | --- | --- |
| **stdio** | `brewops`, `filesystem`, `git` | Proceso hijo en la misma máquina |
| **Streamable HTTP** | `netprobe` | Contenedor, accesible por red |

La pregunta "qué sucede en cada capa" tiene respuestas radicalmente distintas
para cada uno, y ese contraste es el hallazgo central de este análisis: **un
transporte no usa la pila de red en absoluto.**

### 1.1 Método

La evidencia se obtuvo inspeccionando los procesos en ejecución:

- `/proc/<pid>/fd` para ver qué descriptores abre cada servidor.
- `ss -tnp` para contar sockets y conexiones TCP por proceso.
- `ip link` y `docker inspect` para la configuración de enlace y red.
- El registro de frames del anfitrión (`logs/mcp-*.log`) para los tamaños reales
  de los mensajes.
- El propio servidor `netprobe`, cuyas herramientas reportan lo que observan al
  resolver nombres, abrir conexiones TCP e intercambiar peticiones HTTP.

---

## 2. Transporte stdio: las capas que no existen

Un servidor stdio se ejecuta como proceso hijo del anfitrión. Los frames viajan
por la entrada y salida estándar del proceso.

Inspeccionando `brewops` lanzado exactamente como lo lanza el anfitrión:

```
PID 187356
  fd 0 -> pipe:[580772]
  fd 1 -> pipe:[580774]
  sockets abiertos:   0
  conexiones de red:  0
```

Los descriptores 0 y 1 apuntan a **pipes anónimos del kernel**, identificados por
su número de inodo. El proceso no tiene un solo socket abierto.

Esto significa que, para los tres servidores locales del proyecto:

| Capa | Qué ocurre |
| --- | --- |
| **Enlace** | Nada. No hay trama, no hay dirección MAC, no hay medio físico. |
| **Red** | Nada. No hay paquete IP, no hay direccionamiento, no hay enrutamiento. |
| **Transporte** | Nada en el sentido TCP/IP. No hay puerto, ni número de secuencia, ni control de flujo por ventana. |
| **Aplicación** | JSON-RPC 2.0 sobre un flujo de bytes. |

Lo que sí existe, y hace el trabajo que en el caso remoto harían esas capas, es
un **pipe del kernel**: un búfer en memoria del sistema operativo, con
transferencia fiable y ordenada garantizada por el propio kernel y control de
flujo por bloqueo cuando el búfer se llena.

Esta última propiedad no es teórica y tuvo una consecuencia concreta durante el
desarrollo. Un servidor MCP escribe sus diagnósticos a `stderr`; si el anfitrión
no lee ese pipe, el búfer del sistema operativo se llena, el servidor se bloquea
en su siguiente escritura, y la sesión queda esperando una respuesta que nunca
saldrá. El síntoma aparenta ser un interbloqueo del protocolo y no lo es: es
control de flujo de la capa que sustituye al transporte. El anfitrión dedica una
goroutine exclusivamente a drenar `stderr` por esta razón.

### 2.1 Delimitación de mensajes

Un pipe entrega un flujo de bytes sin fronteras: nada indica dónde termina un
mensaje y empieza el siguiente. El transporte stdio de MCP resuelve esto
**delimitando cada frame con un salto de línea**, lo que impone dos reglas:

1. Un frame no puede contener saltos de línea sin escapar.
2. `stdout` transporta frames y **nada más**. Una sola línea de log en ese flujo
   corrompe el stream para el cliente.

---

## 3. Transporte Streamable HTTP: la pila completa

El servidor `netprobe` corre en un contenedor y se alcanza por red. Aquí sí
participan las cuatro capas.

### 3.1 Capa de enlace

Docker crea una topología de enlace virtual completa:

```
IP contenedor: 172.23.0.2
MAC:           1a:61:60:91:b1:93
gateway:       172.23.0.1
bridge:        br-cbee94fb37cd    MAC 92:a2:3c:52:ff:56
interfaces:    vethf9aa8f5@if2, veth59e8dc1@if2
MTU:           1500
```

El contenedor tiene su propia **dirección MAC** y se conecta mediante un par
`veth` —dos interfaces virtuales unidas extremo a extremo— a un **bridge** que
actúa como conmutador de capa 2. El bridge también tiene su propia MAC. Es una
red Ethernet conmutada, implementada enteramente en software.

El **MTU de 1500 bytes** es el dato de esta capa con consecuencias medibles
arriba, y se trata en la sección de transporte.

### 3.2 Capa de red

| | |
| --- | --- |
| Direccionamiento | IPv4 privado, `172.23.0.0/16` |
| Gateway | `172.23.0.1`, la interfaz del bridge en el host |
| Traducción | El host publica `0.0.0.0:8961` y lo redirige a `172.23.0.2:8080` |

La publicación de puertos de Docker es **NAT**: el contenedor no es directamente
alcanzable desde fuera, y la traducción de direcciones ocurre en el host. Esto es
la misma mecánica que cualquier servicio en la nube usa para exponer un
contenedor con dirección privada al Internet público.

Cuando `netprobe` resuelve un nombre, esta capa se vuelve visible en la salida de
la herramienta:

```
github.com resolved in 132ms
  IPv4: 140.82.112.4
  IPv6: (none)
```

### 3.3 Capa de transporte

TCP, y el servidor escucha explícitamente:

```
LISTEN  0  4096  0.0.0.0:8961  0.0.0.0:*
```

La cola de conexiones pendientes es de 4096. La herramienta `tcp_probe` mide el
establecimiento de conexión directamente:

```
TCP probe of github.com
  443    open    (70ms, local 172.23.0.2:41288)
```

Los 70 ms son el tiempo del *three-way handshake* completo, y el puerto local
efímero `41288` es el que el kernel asignó a ese extremo de la conexión.

**Segmentación, medida sobre frames reales.** El registro del anfitrión muestra
la distribución de tamaños de los mensajes MCP:

```
19 frames | mediana 204 B | máximo 13,077 B
```

El máximo es la respuesta a `tools/list` del servidor de filesystem, que publica
14 herramientas con sus esquemas JSON Schema completos. Con un MTU de 1500 bytes,
el MSS resultante es de 1460 bytes (1500 menos 20 de cabecera IP y 20 de cabecera
TCP), de modo que ese frame requiere **10 segmentos TCP**. El mensaje mediano de
204 bytes entra en uno solo.

Esto ilustra una diferencia real entre los dos transportes: por loopback el MTU
es de 65,536 bytes, y ese mismo frame de 13 KB viaja en un único segmento.

### 3.4 Capa de aplicación

Sobre TCP hay **tres protocolos apilados**, y la herramienta `http_probe` los
reporta todos:

```
HEAD https://github.com
  status:   200 OK (207ms)
  protocol: HTTP/2.0
  tls:      TLS 1.3, cipher TLS_AES_128_GCM_SHA256
  cert:     github.com, expires 2026-09-30
```

| Nivel | Protocolo | Función |
| --- | --- | --- |
| Cifrado | TLS 1.3 | Confidencialidad, integridad y autenticación del extremo mediante certificado |
| Transferencia | HTTP/1.1 o HTTP/2 | Método, cabeceras, códigos de estado |
| Aplicación | MCP sobre JSON-RPC 2.0 | Semántica de herramientas |

En el modelo OSI, TLS ocuparía las capas de sesión y presentación; en el modelo
TCP/IP, que es el que la práctica sigue, todo esto es capa de aplicación.

El certificado del servidor es lo que permite verificar la identidad del extremo.
Este detalle tuvo una consecuencia práctica en el proyecto: la imagen del
contenedor **no puede construirse sobre `scratch`**, porque una imagen sin
certificados raíz falla toda conexión TLS con un error de autoridad no
verificable que aparenta ser un fallo de red.

---

## 4. Qué hace MCP en la capa de aplicación

Esto es común a ambos transportes, y es donde vive el protocolo implementado.

### 4.1 Correlación de peticiones y respuestas

JSON-RPC permite tener varias peticiones en vuelo simultáneamente. Cada una
lleva un `id` que la respuesta debe devolver sin modificar, y el cliente mantiene
una tabla que asocia cada `id` pendiente con quien espera esa respuesta.

Esta es la misma función que cumple el número de secuencia en TCP, resuelta de
nuevo un nivel más arriba: el transporte garantiza que los bytes lleguen
ordenados, pero no que las *respuestas* lleguen en el orden en que se pidieron.
El servidor puede responder la tercera petición antes que la primera, y sin la
correlación por `id` cada respuesta llegaría al solicitante equivocado.

### 4.2 Cabeceras propias del transporte HTTP

El transporte HTTP añade dos cabeceras que el transporte stdio no necesita:

| Cabecera | Función |
| --- | --- |
| `Mcp-Session-Id` | Emitida por el servidor en la respuesta a `initialize` y reenviada por el cliente en cada petición posterior. Sustituye lo que en stdio da la propia existencia del proceso hijo. |
| `MCP-Protocol-Version` | La versión acordada, enviada a partir de `initialize`. |

La necesidad de la primera ilustra bien la diferencia entre transportes: en stdio
la sesión **es** el proceso, y termina cuando el proceso termina. Sobre HTTP, que
no tiene estado, la sesión debe construirse explícitamente.

### 4.3 Terminación de sesión

| Transporte | Cómo termina |
| --- | --- |
| stdio | El anfitrión cierra `stdin`; el servidor ve EOF y termina |
| HTTP | El cliente envía `DELETE /mcp` con el identificador de sesión |

En el segundo caso la notificación es necesaria porque una sesión que el servidor
sigue creyendo abierta retiene recursos del otro lado indefinidamente.

---

## 5. Comparación

| Capa | stdio | Streamable HTTP |
| --- | --- | --- |
| Enlace | — | Ethernet virtual, par `veth` sobre bridge, MTU 1500 |
| Red | — | IPv4 privado con NAT, `172.23.0.2` |
| Transporte | Pipe del kernel: fiable, ordenado, control de flujo por bloqueo | TCP: puertos, handshake de 70 ms medido, segmentación por MSS de 1460 B |
| Aplicación | JSON-RPC 2.0 delimitado por saltos de línea | TLS 1.3 + HTTP/2 + JSON-RPC 2.0 |
| Sesión | El proceso hijo | `Mcp-Session-Id` explícito |
| Alcance | Misma máquina | Cualquier red |

La conclusión de la comparación es que **MCP define la misma semántica sobre dos
pilas que no comparten nada por debajo de la capa de aplicación**. El cliente
JSON-RPC implementado en este proyecto no distingue una de otra: ambas
implementan la misma interfaz de tres operaciones —escribir un frame, leer un
frame, cerrar— y todo lo que está por encima es idéntico.

---

## 6. Conclusiones

**El protocolo cumple lo que promete, y se puede comprobar.** El enunciado abre
señalando que cada empresa define su propia forma de integrar herramientas y que
por eso no hay interoperabilidad. El anfitrión desarrollado conecta
simultáneamente cuatro servidores escritos en tres lenguajes —TypeScript, Python
y Go— por dos organizaciones distintas, sobre dos transportes distintos, y le
entrega sus 38 herramientas a un modelo de Google. No se escribió una sola línea
de adaptación para ninguno. Que los servidores oficiales de Anthropic funcionen
con un modelo que no es de Anthropic es la demostración más directa del argumento.

**Implementar el protocolo a mano enseña lo que un SDK esconde.** La decisión de
escribir JSON-RPC directamente, sin SDK de MCP, obligó a resolver problemas que
de otro modo habrían quedado invisibles: la correlación de identificadores, la
distinción entre un fallo de protocolo y un fallo de herramienta, el drenaje de
`stderr` para no bloquear al servidor, la negociación de versión con servidores
de revisiones anteriores. Ninguno de estos aparece en la documentación como una
advertencia destacada; todos aparecen al primer contacto con un servidor real.

**La distinción más importante del protocolo es también la más fácil de
implementar mal.** Una herramienta que se ejecutó y falló no viaja como error de
JSON-RPC, sino como respuesta exitosa con `isError`. El destinatario de ese
fallo es el modelo, que puede leerlo y elegir otro camino; tratarlo como
excepción termina la conversación por un problema que era recuperable. El primer
servidor oficial contra el que se probó el cliente confirmó esta distinción en la
primera llamada.

**Las capas inferiores solo son observables cuando existen.** El resultado más
instructivo del análisis fue descubrir que tres de los cuatro servidores no usan
la pila de red en absoluto: cero sockets, cero conexiones, dos pipes del kernel.
Un análisis por capas de esos servidores no es un análisis corto, es un análisis
vacío. El valor pedagógico está justamente en el contraste con el servidor
remoto, porque muestra qué trabajo desaparece —y quién lo hace en su lugar—
cuando no hay red de por medio.

**Las decisiones de infraestructura tienen consecuencias en la capa de
aplicación.** Elegir `scratch` como imagen base habría producido un contenedor
funcional en todo salvo en las conexiones TLS, fallando con un error que aparenta
ser de red y no lo es. Compilar sin `CGO_ENABLED=0` habría producido una imagen
que construye correctamente y falla al arrancar. Ninguno de los dos errores se
manifiesta donde se origina.

**Lo que se haría distinto.** La implementación cubre únicamente la superficie de
herramientas del protocolo; `resources` y `prompts` quedaron fuera
deliberadamente, y un anfitrión completo debería soportarlas. El transporte HTTP
implementa la lectura de flujos de eventos pero el servidor propio siempre
responde con un frame único, de modo que esa ruta está probada contra un servidor
de prueba y no contra uno que realmente transmita por etapas. Finalmente, la
gestión de contexto es acumulativa: una conversación larga terminará excediendo
la ventana del modelo, y un anfitrión de producción necesitaría resumir o recortar
el historial.
