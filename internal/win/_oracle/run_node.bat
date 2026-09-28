@echo off
rem Lanza el server node en la MISMA consola que este cmd, para armar el caso
rem de "consola compartida con un intruso" (el cmd es el intruso).
"C:\Program Files\nodejs\node.exe" "%~dp0sigint-server.js" %1 0
