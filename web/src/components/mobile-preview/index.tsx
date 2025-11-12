import { useState } from "react";
import './style.css';

const MobilePreview = () => {
  const [notifications, setNotifications] = useState([
    {
      id: 1,
      appName: "MyApp",
      appIcon: "M",
      iconBg: "#4285f4",
      title: "New Message from John",
      text: "Hey! Are you coming to the meeting at 3 PM? Let me know if you need the link.",
      time: "now",
      actions: ["Reply", "Mark Read"],
      type: "standard",
    },
    {
      id: 2,
      appName: "Email",
      appIcon: "Envelope",
      iconBg: "#34a853",
      title: "Meeting Reminder",
      text: "Your meeting starts in 15 minutes",
      time: "5m ago",
      type: "compact",
    },
    {
      id: 3,
      appName: "Notifications",
      appIcon: "Bell",
      iconBg: "#ea4335",
      title: "Order Shipped",
      text: "Your package is on the way!",
      time: "10m ago",
      expandedContent:
        "Tracking: 1Z999AA10123456784\nEstimated delivery: Tomorrow by 8 PM",
      type: "expanded",
    },
  ]);

  const [notificationCount, setNotificationCount] = useState(3);

  const addNotification = () => {
    const randomColor =
      "#" +
      Math.floor(Math.random() * 16777215)
        .toString(16)
        .padStart(6, "0");
    const newNotification = {
      id: Date.now(),
      appName: "Demo App",
      appIcon: "Sparkles",
      iconBg: randomColor,
      title: `Test Notification ${notificationCount + 1}`,
      text: "This is a dynamically added notification to test the UI.",
      time: "just now",
      type: "standard",
    };

    setNotifications([newNotification, ...notifications]);
    setNotificationCount((prev) => prev + 1);
  };

  const clearNotifications = () => {
    setNotifications([]);
    setNotificationCount(0);
  };

  return (
    <>
      {/* Background */}
      <div className="min-h-screen bg-background flex flex-col items-center justify-center p-6">
        {/* Controls */}
        <div className="mb-8 space-x-4">
          <button
            onClick={addNotification}
            className="bg-primary text-primary-foreground px-4 py-2 rounded-lg font-medium shadow hover:shadow-md transition-all duration-200"
          >
            Add Notification
          </button>
          <button
            onClick={clearNotifications}
            className="bg-destructive text-destructive-foreground px-4 py-2 rounded-lg font-medium shadow hover:shadow-md transition-all duration-200"
          >
            Clear All
          </button>
        </div>

        {/* Device Mockup */}
        <div className="device-google-pixel-6-pro">
          <div className="device-frame">
            <div className="device-stripe"></div>
            <div className="device-sensors"></div>
            <div className="device-btns"></div>
            <div className="device-power"></div>
            <div className="device-screen">
              <div className="screen-content">
                {/* Status Bar */}
                <div className="status-bar">
                  <div className="time">10:42</div>
                  <div className="battery">
                    <span>95%</span>
                    <span>📶</span>
                    <span>📶</span>
                  </div>
                </div>

                {/* Notifications */}
                <div className="notifications-container">
                  {notifications.map((notif) => (
                    <div key={notif.id} className={`notification ${notif.type}`}>
                      {/* Header */}
                      <div className="notification-header">
                        <div
                          className="app-icon"
                          style={{ backgroundColor: notif.iconBg }}
                        >
                          {notif.appIcon}
                        </div>
                        <div className="app-name">{notif.appName}</div>
                        <div className="time">{notif.time}</div>
                      </div>

                      {/* Body */}
                      <div className="notification-title">{notif.title}</div>
                      <div className="notification-text">{notif.text}</div>

                      {/* Expanded */}
                      {notif.expandedContent && (
                        <div className="expanded-content">
                          <pre>{notif.expandedContent}</pre>
                        </div>
                      )}

                      {/* Actions */}
                      {notif.actions && (
                        <div className="notification-actions">
                          {notif.actions.map((action, idx) => (
                            <button key={idx} className="action-btn">
                              {action}
                            </button>
                          ))}
                        </div>
                      )}
                    </div>
                  ))}
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </>
  );
};

export default MobilePreview;