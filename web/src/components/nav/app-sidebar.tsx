'use client'

import { useAtom } from 'jotai'
import {
  GalleryVerticalEnd,
  Map,
  PieChart,
} from 'lucide-react'
import * as React from 'react'
import { useLocation, Link } from 'wouter'
import { batchGetAtom } from '@/atoms/projects'

import { NavUser } from '@/components/nav/nav-user'
import { ProjectSwitcher } from '@/components/nav/project-switcher'
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuItem,
  SidebarMenuButton,
  SidebarRail,
} from '@/components/ui/sidebar'

export function AppSidebar({ ...props }: React.ComponentProps<typeof Sidebar>) {
  const [projects] = useAtom(batchGetAtom)
  const [location] = useLocation()

  const projectSwitcherProjects = React.useMemo(() => {
    return projects.map(project => ({
      id: project.id,
      name: project.displayName,
      logo: GalleryVerticalEnd,
      plan: 'Enterprise'
    }))
  }, [projects])

  const isActive = (route: string) => location.startsWith(route)

  return (
    <Sidebar collapsible="icon" {...props}>
      <SidebarHeader>
        <ProjectSwitcher projects={projectSwitcherProjects} />
      </SidebarHeader>
      <SidebarContent>
        <SidebarGroup>
          <SidebarMenu>
            <SidebarMenuItem>
              <SidebarMenuButton asChild isActive={isActive('/campaigns')}>
                <Link to="/campaigns">
                  <PieChart />
                  <span>Campaigns</span>
                </Link>
              </SidebarMenuButton>
            </SidebarMenuItem>
            <SidebarMenuItem>
              <SidebarMenuButton asChild isActive={isActive('/journeys')}>
                <Link to="/journeys">
                  <Map />
                  <span>Journeys</span>
                </Link>
              </SidebarMenuButton>
            </SidebarMenuItem>
            <SidebarMenuItem>
              <SidebarMenuButton asChild isActive={isActive('/projects')}>
                <Link to="/projects">
                  <GalleryVerticalEnd />
                  <span>Projects</span>
                </Link>
              </SidebarMenuButton>
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarGroup>
      </SidebarContent>
      <SidebarFooter>
        <NavUser user={{
          name: 'shadcn',
          email: 'm@example.com',
          avatar: '/avatars/shadcn.jpg',
        }} />
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  )
}
