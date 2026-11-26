# NEXUS GRID miner agent - stop all instances.
$procs = Get-Process "nexus-miner-agent" -ErrorAction SilentlyContinue
if ($procs) {
    $procs | Stop-Process -Force
    Write-Host "miner agent(s) stopped ($($procs.Count))"
} else {
    Write-Host "no miner agent running"
}
