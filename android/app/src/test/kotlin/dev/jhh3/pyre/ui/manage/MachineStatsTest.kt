package dev.jhh3.pyre.ui.manage

import dev.jhh3.pyre.model.DiskUsage
import dev.jhh3.pyre.model.NodeResources
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class MachineStatsTest {
    @Test
    fun olderPyredShowsNoStatsLine() {
        assertNull(machineStatsLine(NodeResources(cpuCount = 8, load1 = 1.0, memTotalMb = 16384, memAvailMb = 8000)))
        assertTrue(machineWarnings(NodeResources(memTotalMb = 16384)).isEmpty())
    }

    @Test
    fun statsLineAndWarnings() {
        val r = NodeResources(
            cpuCount = 10, cpuPct = 18.5, memTotalMb = 16384, memAvailMb = 6100,
            swapTotalMb = 9216, swapUsedMb = 7816, memPressure = "warn",
            disks = listOf(DiskUsage(path = "/Users/synth", roles = listOf("home", "data"), totalMb = 471482, freeMb = 15510)),
        )
        assertEquals("CPU 19% · memory 63% · swap 7.6 GB of 9.0 GB · disk 15.1 GB free", machineStatsLine(r))
        assertEquals(listOf("low disk: 15.1 GB free", "memory pressure warn", "heavy swap"), machineWarnings(r))
    }
}
