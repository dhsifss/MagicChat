package cloud.baizhi.push;

import java.lang.reflect.Field;
import java.util.Collection;
import java.util.Map;
import java.util.List;
import org.junit.Before;
import org.junit.Test;

import static org.junit.Assert.*;

public class NativePushRegistryTest {
  @Before public void reset() throws Exception {
    NativePushRegistry.listener = null;
    for (String name : new String[]{"tokens", "pending", "unsupported"}) {
      Field field = NativePushRegistry.class.getDeclaredField(name);
      field.setAccessible(true);
      Object value = field.get(null);
      if (value instanceof Map) ((Map<?, ?>) value).clear();
      else ((Collection<?>) value).clear();
    }
  }

  @Test public void supportRejectionCompletesLookupAndRemovesStaleToken() {
    NativePushRegistry.token("honor_push", "old-token");
    NativePushRegistry.pending("honor_push");
    NativePushRegistry.unsupported("honor_push");
    assertFalse(NativePushRegistry.isPending());
    assertEquals(List.of("honor_push"), NativePushRegistry.unsupportedProviders());
    assertTrue(NativePushRegistry.snapshot().isEmpty());
  }

  @Test public void temporaryFailureKeepsTokenAndDoesNotClaimUnsupported() {
    NativePushRegistry.token("honor_push", "working-token");
    NativePushRegistry.pending("honor_push");
    NativePushRegistry.complete("honor_push");
    assertFalse(NativePushRegistry.isPending());
    assertTrue(NativePushRegistry.unsupportedProviders().isEmpty());
    assertEquals("working-token", NativePushRegistry.snapshot().get(0).get("token"));
  }

  @Test public void supportIsReevaluatedAndLaterRegistrationCanRecover() {
    NativePushRegistry.unsupported("honor_push");
    NativePushRegistry.pending("honor_push");
    assertTrue(NativePushRegistry.unsupportedProviders().isEmpty());
    assertTrue(NativePushRegistry.isPending());
    NativePushRegistry.token("honor_push", "recovered-token");
    assertFalse(NativePushRegistry.isPending());
    assertTrue(NativePushRegistry.unsupportedProviders().isEmpty());
    assertEquals("recovered-token", NativePushRegistry.snapshot().get(0).get("token"));
  }
}
