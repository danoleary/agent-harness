This is a .NET solution. Before you commit a fix, run `dotnet build` and the
test project closest to the files you changed. Keep `dotnet format` clean: the
harness re-runs it as a gate and refuses a fix that fails it.
