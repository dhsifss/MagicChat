package cloud.baizhi.push;

import android.content.Context;
import android.content.Intent;
import android.os.Bundle;
import com.xiaomi.mipush.sdk.MiPushMessage;
import java.lang.reflect.Field;
import java.util.Set;
import java.util.concurrent.atomic.AtomicInteger;
import org.junit.Before;
import org.junit.Test;
import org.junit.runner.RunWith;
import org.robolectric.RobolectricTestRunner;
import org.robolectric.RuntimeEnvironment;
import org.robolectric.annotation.Config;

import static org.junit.Assert.*;

@RunWith(RobolectricTestRunner.class)
// The OEM jar omits JVM stack maps; Robolectric regenerates them while instrumenting.
@Config(manifest = Config.NONE, sdk = 35, instrumentedPackages = {"com.xiaomi.mipush.sdk"})
public class NativePushResponseTest {
  private static final String GRANT = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
  private static final String TOKEN = "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr";
  private static final String PAYLOAD = "{\"event\":\"message_created\",\"grant_id\":\"" + GRANT + "\",\"route_token\":\"" + TOKEN + "\"}";
  private Context context;

  @Before public void reset() throws Exception {
    context = RuntimeEnvironment.getApplication();
    context.getSharedPreferences("magicchat_push_responses", Context.MODE_PRIVATE).edit().clear().commit();
    resetProcessState();
    NativePushResponse.initialize(context);
  }

  private void resetProcessState() throws Exception {
    NativePushResponse.listener = null;
    for (String name : new String[]{"last", "preferences"}) {
      Field field = NativePushResponse.class.getDeclaredField(name);
      field.setAccessible(true);
      field.set(null, null);
    }
    Field field = NativePushResponse.class.getDeclaredField("consumed");
    field.setAccessible(true);
    ((Set<?>) field.get(null)).clear();
  }

  @Test public void xiaomiLaunchMessageRoutesToConversation() {
    MiPushMessage message = new MiPushMessage();
    message.setContent(PAYLOAD);
    Intent intent = new Intent().putExtra("key_message", message);
    NativePushResponse.fromIntent(intent);
    assertNotNull(NativePushResponse.last());
    assertEquals(GRANT + ":" + TOKEN, NativePushResponse.last().get("identifier"));
  }

  @Test public void consumedIntentStaysConsumedAfterProcessRestart() throws Exception {
    Bundle data = new Bundle();
    data.putString("event", "message_created");
    data.putString("grant_id", GRANT);
    data.putString("route_token", TOKEN);
    Intent[] intents = {
      new Intent().putExtras(data),
      new Intent().putExtra("payload", PAYLOAD),
      new Intent().putExtra("data", data),
      new Intent().putExtra("key_message", xiaomiMessage()),
    };
    NativePushResponse.fromIntent(intents[0]);
    NativePushResponse.clear();
    AtomicInteger responses = new AtomicInteger();
    NativePushResponse.listener = response -> responses.incrementAndGet();
    for (Intent intent : intents) NativePushResponse.fromIntent(intent);
    assertNull(NativePushResponse.last());
    assertEquals(0, responses.get());

    resetProcessState();
    // An SDK receiver may deliver before Expo initializes the module.
    NativePushResponse.fromIntent(intents[3]);
    NativePushResponse.initialize(context);
    NativePushResponse.listener = response -> responses.incrementAndGet();
    for (Intent intent : intents) NativePushResponse.fromIntent(intent);
    assertNull(NativePushResponse.last());
    assertEquals(0, responses.get());

    Intent nextTap = new Intent().putExtra("payload", PAYLOAD.replace(TOKEN, "s" + TOKEN));
    NativePushResponse.fromIntent(nextTap);
    assertNotNull(NativePushResponse.last());
    assertEquals(1, responses.get());
  }

  @Test public void invalidXiaomiContentDoesNotHideValidIntentData() {
    MiPushMessage message = new MiPushMessage();
    message.setContent("{}");
    NativePushResponse.fromIntent(new Intent().putExtra("key_message", message).putExtra("payload", PAYLOAD));
    assertNotNull(NativePushResponse.last());
  }

  private MiPushMessage xiaomiMessage() {
    MiPushMessage message = new MiPushMessage();
    message.setContent(PAYLOAD);
    return message;
  }
}
