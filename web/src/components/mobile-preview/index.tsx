import './style.css'

export interface Notification {
  id: number;
  appName: string;
  appIcon: string;
  iconBg: string;
  title: string;
  text: string;
  time: string;
  actions?: string[];
  type: 'standard' | 'compact' | 'expanded';
  expandedContent?: string;
}

/*
  {
    id: 1,
    appName: 'MyApp',
    appIcon: 'M',
    iconBg: '#4285f4',
    title: titleValue || 'Notification Title',
    text: bodyValue || 'Notification body will appear here',
    time: 'now',
    actions: ['Reply', 'Mark Read'],
    type: 'standard' as const,
  },
  {
    id: 2,
    appName: 'Promotions',
    appIcon: 'P',
    iconBg: '#ff6b6b',
    title: titleValue || 'Special Offer Inside',
    text: bodyValue || 'Get 20% off your next purchase with code WELCOME20',
    time: '5m ago',
    type: 'compact' as const,
  },
  {
    id: 3,
    appName: 'Updates',
    appIcon: 'U',
    iconBg: '#4ecdc4',
    title: titleValue || 'New Features Available',
    text: bodyValue || 'Check out our latest features that will enhance your experience.',
    time: '10m ago',
    expandedContent: 'New Dashboard: Improved analytics view\n' +
      'Enhanced Security: Two-factor authentication\n' +
      'Better Performance: 30% faster loading times',
    type: 'expanded' as const,
  }
*/

interface MobilePreviewProps {
  notifications?: Notification[];
}

const MobilePreview: React.FC<MobilePreviewProps> = ({
  notifications = [],
}) => {

  function getCurrentTime() {
    const now = new Date()
    return now.toLocaleTimeString('en-US', { 
      hour: '2-digit', 
      minute: '2-digit', 
      hour12: false
    })
  }

  return (
    <>
      {/* Background */}
      <div className="w-full bg-background flex flex-col items-center">
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
                  <div className="time">{getCurrentTime()}</div>
                  <div className="battery">
                    <span>80%</span>
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
  )
}

export default MobilePreview